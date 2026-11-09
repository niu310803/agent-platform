package voice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"agent-platform/internal/chat"
	"agent-platform/internal/contracts"
	rt "agent-platform/internal/runtime/types"
	"agent-platform/internal/stream"
	"github.com/gorilla/websocket"
)

type recoveryRuntime struct {
	Runtime
	execute   func(context.Context, rt.QueryCommand, rt.QueryHooks) (rt.QueryResult, error)
	attach    func() *rt.Subscription
	interrupt func() (rt.InterruptResult, error)
	status    func(string) (contracts.RunSnapshot, error)
	steer     func(context.Context, rt.SteerCommand) (rt.SteerResult, error)
}

func (r recoveryRuntime) ExecuteQueryWithHooks(ctx context.Context, cmd rt.QueryCommand, hooks rt.QueryHooks) (rt.QueryResult, error) {
	return r.execute(ctx, cmd, hooks)
}
func (r recoveryRuntime) AttachRun(context.Context, rt.RunRef, int64) (*rt.Subscription, error) {
	return r.attach(), nil
}
func (r recoveryRuntime) Interrupt(context.Context, rt.InterruptCommand) (rt.InterruptResult, error) {
	return r.interrupt()
}

func (r recoveryRuntime) Steer(ctx context.Context, cmd rt.SteerCommand) (rt.SteerResult, error) {
	return r.steer(ctx, cmd)
}

type recoveryRuns struct{ active string }

func (r recoveryRuns) ActiveRunForChat(string) (contracts.RunStatusInfo, bool, error) {
	return contracts.RunStatusInfo{RunID: r.active}, r.active != "", nil
}

func TestVoiceInterruptRetriesFailureButCoalescesPendingAndAccepted(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "error", true: "rejected"}[rejected], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls atomic.Int32
				release := make(chan struct{})
				runtime := recoveryRuntime{interrupt: func() (rt.InterruptResult, error) {
					if calls.Add(1) == 1 {
						<-release
						if rejected {
							return rt.InterruptResult{}, nil
						}
						return rt.InterruptResult{}, errors.New("proxy unavailable")
					}
					return rt.InterruptResult{Accepted: true}, nil
				}}
				service := NewService(context.Background(), Dependencies{Runtime: runtime, Runs: recoveryRuns{active: "owned"}})
				defer service.Close()
				s := &session{service: service, ctx: service.ctx, events: make(chan sessionEvent, 16)}
				run := &ownedRun{}
				run.cancelled.Store(true)
				s.interrupt(run) // A late RunID must remain cancellable.
				if calls.Load() != 0 {
					t.Fatal("interrupted without identity")
				}
				run.ref.Store(&rt.RunRef{RunID: "owned", ChatID: "chat"})
				s.interrupt(run)
				synctest.Wait()
				s.interrupt(run)
				if calls.Load() != 1 {
					t.Fatal("duplicated in-flight interrupt")
				}
				close(release)
				synctest.Wait()
				if e := <-s.events; e.kind != "interruptFailed" || e.run != run {
					t.Fatal(e)
				}
				s.interrupt(run)
				synctest.Wait()
				s.interrupt(run)
				synctest.Wait()
				if calls.Load() != 2 || run.interruptState.Load() != interruptAccepted {
					t.Fatalf("calls=%d state=%d", calls.Load(), run.interruptState.Load())
				}
			})
		})
	}
}

func TestVoiceInterruptAlreadyFinishedIsNotRetried(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		service := NewService(context.Background(), Dependencies{Runtime: recoveryRuntime{interrupt: func() (rt.InterruptResult, error) { calls.Add(1); return rt.InterruptResult{Accepted: false}, nil }}, Runs: recoveryRuns{}})
		defer service.Close()
		s := &session{service: service, ctx: service.ctx, events: make(chan sessionEvent, 16)}
		run := &ownedRun{}
		run.cancelled.Store(true)
		run.ref.Store(&rt.RunRef{RunID: "finished", ChatID: "chat"})
		s.interrupt(run)
		synctest.Wait()
		s.interrupt(run)
		synctest.Wait()
		if calls.Load() != 1 || len(s.events) != 0 {
			t.Fatal("finished Run treated as a failed interrupt")
		}
	})
}

func TestVoiceBoundsObservationAfterQueryReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closed := false
		runtime := recoveryRuntime{
			execute: func(_ context.Context, _ rt.QueryCommand, hooks rt.QueryHooks) (rt.QueryResult, error) {
				hooks.OnRunStarted(chat.RunStart{RunID: "run", ChatID: "chat"})
				return rt.QueryResult{}, errors.New("registration failed")
			},
			attach: func() *rt.Subscription {
				return rt.NewSubscription("observer", make(chan stream.EventData), func() { closed = true })
			},
		}
		service := NewService(context.Background(), Dependencies{Runtime: runtime, Runs: recoveryRuns{}})
		defer service.Close()
		ctx, cancel := context.WithCancel(service.ctx)
		defer cancel()
		s := &session{service: service, ctx: ctx, chatID: "chat", events: make(chan sessionEvent, 16)}
		turnCtx, turnCancel := context.WithCancel(ctx)
		defer turnCancel()
		turn := &turn{ctx: turnCtx, cancel: turnCancel, number: 1, text: "question"}
		s.current = turn
		if err := s.submit(turn); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if e := <-s.events; e.kind != "runStarted" {
			t.Fatal(e)
		}
		time.Sleep(finishTimeout)
		synctest.Wait()
		select {
		case e := <-s.events:
			if e.kind != "releaseFailed" || !closed {
				t.Fatalf("event=%s closed=%v", e.kind, closed)
			}
		default:
			t.Fatal("observer wait exceeded the finish deadline")
		}
	})
}

// The event loop writes only to this ephemeral socket; no application or
// provider is started, and all state belongs to the test.
func eventSession(t *testing.T, deps Dependencies) (*session, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		accepted <- c
	}))
	t.Cleanup(server.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	conn := <-accepted
	if deps.Usage == nil {
		deps.Usage = &testUsageWriter{}
	}
	service := NewService(context.Background(), deps)
	s := &session{asr: &ASRClient{input: audioFormat{Codec: "pcm_s16le", SampleRate: 16000, Channels: 1, FrameMS: 20}}, service: service, conn: conn, ctx: service.ctx, events: make(chan sessionEvent, 16), chatID: "chat", nextTurn: 2}
	t.Cleanup(func() { s.invalidate(); s.clearPending(); service.Close(); conn.Close(); client.Close() })
	return s, client
}

func eventFrame(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var frame map[string]any
	if err := c.ReadJSON(&frame); err != nil {
		t.Fatal(err)
	}
	return frame
}

func mediaTurn(t *testing.T, number int) *turn {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return &turn{ctx: ctx, cancel: cancel, number: number, phase: "listening", started: time.Now(), asrTrace: &taskTrace{}}
}

func TestVoiceRunObservationsSurviveMediaTurn(t *testing.T) {
	s, client := eventSession(t, Dependencies{Runtime: recoveryRuntime{status: func(string) (contracts.RunSnapshot, error) {
		return contracts.RunSnapshot{ChatID: "chat", AwaitingCount: 1, Awaiting: &contracts.RunAwaiting{AwaitingID: "ask", Mode: "question", Questions: []any{map[string]any{"id": "q", "type": "text"}}}}, nil
	}}})
	s.chatID = "chat"
	old, current := mediaTurn(t, 1), mediaTurn(t, 2)
	old.cancel()
	s.current = current
	run := &ownedRun{}
	run.ref.Store(&rt.RunRef{RunID: "owned", ChatID: "chat"})
	s.running = run
	for _, kind := range []string{"runStarted", "awaiting.ask"} {
		if err := s.event(sessionEvent{kind: kind, turn: old, run: run, awaitingID: "ask", mode: "question"}); err != nil {
			t.Fatal(err)
		}
		frame := eventFrame(t, client)
		if frame["runId"] != "owned" || frame["turn"] != nil {
			t.Fatal(frame)
		}
	}
	if current.phase != "listening" || current.speech != nil || run.cancelled.Load() {
		t.Fatal("Run observation interrupted new input")
	}
	// An event from another Run must not alter this session's owner.
	for _, kind := range []string{"runResult", "releaseFailed"} {
		if err := s.event(sessionEvent{kind: kind, turn: current, run: &ownedRun{}}); err != nil {
			t.Fatal(err)
		}
	}
	if s.running != run {
		t.Fatal("foreign result released owned Run")
	}
}

func TestVoiceResultDuringInputRetainedUntilInputResolves(t *testing.T) {
	s, client := eventSession(t, Dependencies{})
	old, current := mediaTurn(t, 1), mediaTurn(t, 2)
	old.cancel()
	s.current = current
	run := &ownedRun{}
	s.running = run
	// A failed result is also retained, rather than interrupting microphone input.
	result := rt.QueryResult{RunID: "owned", FinishReason: "error"}
	if err := s.event(sessionEvent{kind: "runResult", turn: old, run: run, result: result}); err != nil {
		t.Fatal(err)
	}
	if frame := eventFrame(t, client); frame["state"] != "end" {
		t.Fatal(frame)
	}
	if s.completed == nil || s.running != nil || current.phase != "listening" || current.speech != nil {
		t.Fatal("result lost or input interrupted")
	}
	if err := s.event(sessionEvent{kind: "recognized", turn: current}); err != nil {
		t.Fatal(err)
	}
	if frame := eventFrame(t, client); frame["type"] != "stt" || frame["final"] != true {
		t.Fatal(frame)
	}
	if frame := eventFrame(t, client); frame["code"] != "run_failed" {
		t.Fatal(frame)
	}
	if s.completed != nil {
		t.Fatal("result can be delivered twice")
	}
}

func TestVoiceOldMediaCallbacksAreAcknowledgedAndDropped(t *testing.T) {
	s, _ := eventSession(t, Dependencies{})
	old := mediaTurn(t, 1)
	old.cancel()
	s.current = mediaTurn(t, 2)
	for _, kind := range []string{"partial", "recognized", "pcm", "spoken"} {
		ack := make(chan error, 1)
		if err := s.event(sessionEvent{kind: kind, turn: old, text: "stale", ack: ack}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ack:
		default:
			t.Fatal("producer left waiting")
		}
	}
	if s.current.text != "" || s.current.phase != "listening" || s.running != nil {
		t.Fatal("old callback mutated new turn")
	}
}

type resultChats struct {
	ChatReader
	summary *chat.Summary
}

func (r resultChats) Summary(string) (*chat.Summary, error) { return r.summary, nil }

func TestVoiceRetainedFinalRechecksChatBeforePlayback(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{true: "new awaiting", false: "new Run"}[pending], func(t *testing.T) {
			summary := &chat.Summary{}
			runs := recoveryRuns{active: "new-run"}
			if pending {
				summary.PendingAwaiting = &chat.PendingAwaiting{AwaitingID: "new-ask"}
				runs.active = ""
			}
			s, client := eventSession(t, Dependencies{Chats: resultChats{summary: summary}, Runs: runs})
			s.current = mediaTurn(t, 2)
			s.completed = &sessionEvent{result: rt.QueryResult{RunID: "owned", Persisted: true, FinishReason: "complete", Completion: &chat.RunCompletion{AssistantText: "final"}}}
			if err := s.event(sessionEvent{kind: "recognized", turn: s.current}); err != nil {
				t.Fatal(err)
			}
			eventFrame(t, client)
			frame := eventFrame(t, client)
			if frame["type"] != "error" || s.current.speech != nil || s.completed != nil {
				t.Fatal(frame)
			}
		})
	}
}

func TestVoiceNewInputSupersedesUnplayedResultOnce(t *testing.T) {
	queries := make(chan rt.QueryCommand, 2)
	runtime := recoveryRuntime{execute: func(_ context.Context, cmd rt.QueryCommand, _ rt.QueryHooks) (rt.QueryResult, error) {
		queries <- cmd
		return rt.QueryResult{}, errors.New("test ends before admission")
	}}
	s, client := eventSession(t, Dependencies{Runtime: runtime, Runs: recoveryRuns{}, Chats: resultChats{}})
	s.current = mediaTurn(t, 2)
	s.completed = &sessionEvent{result: rt.QueryResult{RunID: "previous"}}
	if err := s.event(sessionEvent{kind: "recognized", turn: s.current, text: "next question"}); err != nil {
		t.Fatal(err)
	}
	if frame := eventFrame(t, client); frame["type"] != "stt" {
		t.Fatal(frame)
	}
	select {
	case query := <-queries:
		if query.Message != "next question" {
			t.Fatal(query.Message)
		}
	case <-time.After(time.Second):
		t.Fatal("new input was not submitted")
	}
	if err := s.event(sessionEvent{kind: "recognized", turn: s.current, text: "next question"}); err != nil {
		t.Fatal(err)
	}
	s.service.Close()
	if len(queries) != 0 {
		t.Fatal("a duplicate final submitted another Query")
	}
	if s.completed != nil || s.current.speech != nil {
		t.Fatal("superseded result was retained or played")
	}
}

func TestVoiceAbortDiscardsUnplayedResult(t *testing.T) {
	s, client := eventSession(t, Dependencies{})
	s.current = mediaTurn(t, 2)
	s.completed = &sessionEvent{result: rt.QueryResult{RunID: "previous"}}
	if err := s.control(clientMessage{Type: "abort"}); err != nil {
		t.Fatal(err)
	}
	if frame := eventFrame(t, client); frame["type"] != "abort" {
		t.Fatal(frame)
	}
	if s.completed != nil || s.current != nil {
		t.Fatal("abort retained media or an unplayed result")
	}
}

func TestVoiceRetainedFinalPlaysOnCurrentTurnAfterEmptyInput(t *testing.T) {
	provider := fakeVoice(t, func(c *websocket.Conn) {
		id := readCommand(t, c, "run-task").Header.ID
		event(t, c, id, "task-started", nil)
		if cmd := readCommand(t, c, "continue-task"); cmd.Payload.Input.Text != "saved final" {
			t.Error("wrong final body")
		}
		readCommand(t, c, "finish-task")
		_ = c.WriteMessage(websocket.BinaryMessage, []byte{1, 2})
		event(t, c, id, "task-finished", nil)
	})
	tts := testTTS(t, provider)
	s, client := eventSession(t, Dependencies{Chats: resultChats{}, Runs: recoveryRuns{}})
	s.tts, s.voice, s.sampleRate = tts, "voice", 16000
	s.current = mediaTurn(t, 2)
	old := mediaTurn(t, 1)
	old.cancel()
	run := &ownedRun{}
	s.running = run
	result := rt.QueryResult{RunID: "owned", Persisted: true, FinishReason: "complete", Completion: &chat.RunCompletion{AssistantText: "saved final"}}
	if err := s.event(sessionEvent{kind: "runResult", run: run, turn: old, result: result}); err != nil {
		t.Fatal(err)
	}
	eventFrame(t, client) // Run end does not start audio while input is active.
	if s.current.speech != nil {
		t.Fatal("played during input")
	}
	if err := s.event(sessionEvent{kind: "recognized", turn: s.current}); err != nil {
		t.Fatal(err)
	}
	eventFrame(t, client) // Empty STT.
	if frame := eventFrame(t, client); frame["type"] != "tts" || frame["turn"] != float64(2) || frame["text"] != "saved final" {
		t.Fatal(frame)
	}
	for {
		select {
		case e := <-s.events:
			if err := s.event(e); err != nil {
				t.Fatal(err)
			}
			if e.kind == "pcm" {
				kind, pcm, err := client.ReadMessage()
				if err != nil || kind != websocket.BinaryMessage || len(pcm) != 2 {
					t.Fatalf("audio: %d %v", kind, err)
				}
			}
			if e.kind == "spoken" {
				if frame := eventFrame(t, client); frame["reason"] != "done" {
					t.Fatal(frame)
				}
				if s.completed != nil {
					t.Fatal("result retained after playback")
				}
				return
			}
		case <-time.After(3 * time.Second):
			t.Fatal("final playback did not finish")
		}
	}
}

func (r recoveryRuntime) GetRunStatus(id string) (contracts.RunSnapshot, error) { return r.status(id) }

func TestVoiceQuestionBindingUsesRuntimeAndNeverFallsBackToQuery(t *testing.T) {
	snapshot := contracts.RunSnapshot{ChatID: "chat", AgentKey: "agent", AwaitingCount: 1, Awaiting: &contracts.RunAwaiting{Mode: "question", AwaitingID: "ask", Questions: []any{map[string]any{"id": "q1", "type": "text"}, map[string]any{"id": "secret", "type": "password"}}}}
	runtime := recoveryRuntime{status: func(string) (contracts.RunSnapshot, error) { return snapshot, nil }}
	s, client := eventSession(t, Dependencies{Runtime: runtime})
	s.agentKey = "agent"
	run := &ownedRun{}
	run.ref.Store(&rt.RunRef{RunID: "owned"})
	s.running = run
	valid := clientMessage{Intent: "question", RunID: "owned", AwaitingID: "ask", QuestionID: "q1"}
	if err := s.validateInput(valid); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*clientMessage){
		func(m *clientMessage) { m.RunID = "foreign" },
		func(m *clientMessage) { m.AwaitingID = "old" },
		func(m *clientMessage) { m.QuestionID = "missing" },
		func(m *clientMessage) { m.QuestionID = "secret" },
	} {
		msg := valid
		change(&msg)
		if err := s.validateInput(msg); err == nil {
			t.Fatal("invalid binding accepted", msg)
		}
	}
	snapshot.Awaiting = nil
	if err := s.validateInput(valid); err == nil {
		t.Fatal("resolved question accepted")
	}
	run.cancelled.Store(true)
	s.current = mediaTurn(t, 2)
	s.current.intent = "question"
	s.current.runID, s.current.awaitingID, s.current.questionID = "owned", "ask", "q1"
	if err := s.event(sessionEvent{kind: "recognized", turn: s.current, text: "answer"}); err != nil {
		t.Fatal(err)
	}
	if frame := eventFrame(t, client); frame["type"] != "awaiting" || frame["state"] != "resolved" || frame["awaitingId"] != "ask" {
		t.Fatal(frame)
	}
	if s.pending != nil {
		t.Fatal("question answer queued as a Query after cancellation")
	}
}
