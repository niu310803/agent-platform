package voice

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"agent-platform/internal/chat"
	"agent-platform/internal/contracts"
	"agent-platform/internal/observability"
	"agent-platform/internal/runtime/controlscope"
	rt "agent-platform/internal/runtime/types"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type turn struct {
	usages        []chat.VoiceUsage
	inputRun      *ownedRun // Frozen at listen.start, including a not-yet-published RunID.
	steered       bool      // A final transcript never sends a second Steer.
	requestID     string
	intent        string
	runID         string
	awaitingID    string
	questionID    string
	number        int
	ctx           context.Context
	cancel        context.CancelFunc
	audio         chan []byte
	phase         string
	awaiting      bool
	text          string
	tailSeen      bool
	speech        *speechTask
	started       time.Time
	inputBytes    int
	inputSamples  int
	queuedSamples atomic.Int64
	queuedBytes   atomic.Int64
	heard         bool
	asrTrace      *taskTrace
}

type speechTask struct {
	runID, awaitingID string
	trace             *taskTrace
	id                string
	purpose           string
	ctx               context.Context
	cancel            context.CancelFunc
	hint              bool
	started           time.Time
	pcmBytes          int
	textChars         int
}

const (
	interruptIdle int32 = iota
	interruptPending
	interruptAccepted
	interruptFailed
)

// ownedRun records only this session's one blocking call and explicit cancel
// intention. Admission, Chat exclusion and HITL remain owned by Runtime.
type ownedRun struct {
	ref            atomic.Pointer[rt.RunRef]
	cancelled      atomic.Bool
	interruptState atomic.Int32
}

type sessionEvent struct {
	finalized        time.Time
	submitResult     rt.SubmitResult
	usages           []chat.VoiceUsage
	usageErr         error
	usageDone        chan struct{}
	kind             string
	turn             *turn
	run              *ownedRun
	speech           *speechTask
	text             string
	stopping         bool
	awaitingID, mode string
	result           rt.QueryResult
	steer            rt.SteerResult
	err              error
	pcm              []byte
	ack              chan error
}

type inbound struct {
	kind int
	data []byte
	err  error
}

type session struct {
	started                 time.Time
	service                 *Service
	conn                    *websocket.Conn
	caller                  Caller
	ctx                     context.Context
	cancel                  context.CancelFunc
	chatID, agentKey, voice string
	timeZone                string
	id                      string
	primary                 bool
	chatCreated             bool
	primaryIfMissing        bool
	sampleRate              int
	asr                     *ASRClient
	tts                     *TTSClient
	events                  chan sessionEvent
	current                 *turn
	running                 *ownedRun
	completed               *sessionEvent // At most one result waiting for current input to resolve.
	pending                 *turn
	waitTimer               *time.Timer
	waited                  <-chan time.Time
	nextTurn                int
}

func runSession(service *Service, conn *websocket.Conn, caller Caller) {
	ctx, cancel := context.WithCancel(service.ctx)
	defer cancel()
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if caller.Identity != nil && caller.Identity.ExpiresAt != "" {
		if expiry, err := time.Parse(time.RFC3339, caller.Identity.ExpiresAt); err == nil {
			timer := time.AfterFunc(max(time.Until(expiry), 0), cancel)
			defer timer.Stop()
		}
	}
	s := &session{service: service, conn: conn, caller: caller, ctx: ctx, cancel: cancel, events: make(chan sessionEvent, 16), id: uuid.NewString(), started: time.Now()}
	conn.SetReadLimit(16 << 10)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	kind, data, err := conn.ReadMessage()
	if err != nil {
		return
	}
	hello, err := decodeMessage(data)
	if kind != websocket.TextMessage || err != nil {
		_ = s.fail("invalid_request", "请先发送语音 hello")
		return
	}
	s.chatID, s.primary, s.voice, err = service.resolveHello(hello)
	if err != nil {
		_ = s.fail("invalid_request", err.Error())
		return
	}
	summary, err := service.deps.Chats.Summary(s.chatID)
	if err != nil {
		_ = s.fail("chat_failed", "聊天状态不可用")
		return
	}
	s.chatCreated = summary != nil
	s.agentKey, s.sampleRate, s.timeZone = hello.AgentKey, hello.Audio.Output.SampleRate, hello.TimeZone
	s.primaryIfMissing = s.primary && !hello.NewChat
	am, ap, tm, tp, err := service.speechModels()
	if err != nil {
		_ = s.fail("voice_unavailable", "语音模型不可用")
		return
	}
	s.asr, err = NewASRClient(am, ap, service.deps.Config.MaxSentenceSilenceMS)
	if err != nil {
		_ = s.fail("voice_unavailable", "语音识别配置不可用")
		return
	}
	defer s.asr.Close()
	s.asr.input = hello.Audio.Input
	if hello.Audio.Input.Codec == "opus" {
		conn.SetReadLimit(maxOpusPacketBytes)
	}
	s.tts, err = NewTTSClient(tm, tp)
	if err != nil {
		_ = s.fail("voice_unavailable", "语音合成配置不可用")
		return
	}
	defer s.tts.Close()
	s.asr.socket.pricing = service.taskPricing(am)
	s.tts.socket.pricing = service.taskPricing(tm)
	if s.write(map[string]any{"type": "hello", "version": 2, "sessionId": s.id, "agentKey": s.agentKey, "chatId": s.chatID, "audio": map[string]any{"input": hello.Audio.Input, "output": audioFormat{Codec: "pcm_s16le", SampleRate: s.sampleRate, Channels: 1, FrameMS: 20}}}) != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	in := make(chan inbound)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			kind, data, err := conn.ReadMessage()
			select {
			case in <- inbound{kind, data, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); _ = conn.Close(); s.invalidate(); s.clearPending(); <-readerDone }()
	for {
		select {
		case <-ctx.Done():
			return
		case input := <-in:
			if input.err != nil {
				return
			}
			if input.kind == websocket.BinaryMessage {
				err = s.audio(input.data)
			} else if input.kind == websocket.TextMessage {
				if len(input.data) > 16<<10 {
					_ = s.protocolError("语音控制消息过大")
					return
				}
				var msg clientMessage
				msg, err = decodeMessage(input.data)
				if err == nil {
					err = s.control(msg)
				} else {
					err = s.protocolError("语音消息格式错误")
				}
			} else {
				err = s.fail("invalid_request", "语音消息类型错误")
			}
			if err != nil {
				return
			}
		case e := <-s.events:
			if s.event(e) != nil {
				return
			}
		case <-s.waited:
			s.clearPending()
			if s.current != nil && s.failTurn(s.current, "run_busy_timeout", "旧任务尚未结束，请稍后重试") != nil {
				return
			}
		}
	}
}

func (s *session) write(value any) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	return s.conn.WriteJSON(value)
}

func (s *session) fail(code, msg string) error {
	observability.Log("voice", map[string]any{"sessionId": s.id, "turn": s.nextTurn, "stage": "error", "code": code})
	return s.write(map[string]any{"type": "error", "turn": s.nextTurn, "code": code, "msg": msg})
}

// failTurn ends media only; Runtime retains ownership of any accepted Run.
func (s *session) failTurn(t *turn, code, message string) error {
	t.cancel()
	s.stopListening(t)
	t.phase, t.text, t.awaiting = "done", "", false
	if s.pending == t {
		s.clearPending()
	}
	return s.fail(code, message)
}

func (s *session) post(ctx context.Context, e sessionEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case <-s.ctx.Done():
		return false
	case s.events <- e:
		return true
	}
}

func (s *session) stopListening(t *turn) {
	if t.audio != nil {
		close(t.audio)
		t.audio = nil
	}
}

func (s *session) invalidate() {
	if s.current != nil {
		s.current.cancel()
		s.stopListening(s.current)
		s.current = nil
	}
}

func (s *session) clearPending() {
	if s.waitTimer != nil {
		s.waitTimer.Stop()
	}
	s.waitTimer, s.waited, s.pending = nil, nil, nil
}

func (s *session) control(msg clientMessage) error {
	switch msg.Type {
	case "abort":
		s.clearCompleted()
		number := s.nextTurn
		s.invalidate()
		s.clearPending()
		if s.running != nil {
			s.running.cancelled.Store(true)
			s.interrupt(s.running)
		}
		// This event loop is the sole writer. Any prior in-flight write has
		// completed or failed before this confirmation can cross the boundary.
		return s.write(map[string]any{"type": "abort", "turn": number})
	case "close":
		s.cancel()
		return nil
	case "listen":
		if msg.State == "stop" {
			if s.current == nil || msg.Turn != s.current.number || msg.RequestID != s.current.requestID {
				return s.protocolError("收音轮次已失效")
			}
			s.stopListening(s.current)
			s.timing("manualStopReceived", time.Now(), map[string]any{"turn": s.current.number})
			return nil
		}
		if msg.State != "start" || msg.Turn != s.nextTurn+1 || msg.RequestID == "" {
			return s.protocolError("listen 轮次或请求标识无效")
		}
		if err := s.validateInput(msg); err != nil {
			if msg.Intent == "question" {
				// A UI answer may win while listen.start is in flight. Consume
				// its media sequence without starting ASR or cancelling the Run.
				s.nextTurn = msg.Turn
				if writeErr := s.write(map[string]any{"type": "error", "turn": msg.Turn, "code": "input_changed", "msg": "当前问题已失效，请查看聊天界面"}); writeErr != nil {
					return writeErr
				}
				return s.resolveQuestionMedia(msg.RunID, msg.AwaitingID)
			}
			return s.protocolError(err.Error())
		}
		s.invalidate()
		s.clearPending()
		s.nextTurn = msg.Turn
		ctx, cancel := context.WithCancel(s.ctx)
		t := &turn{requestID: msg.RequestID, intent: msg.Intent, runID: msg.RunID, awaitingID: msg.AwaitingID, questionID: msg.QuestionID, number: s.nextTurn, ctx: ctx, cancel: cancel, audio: make(chan []byte, s.asr.input.startupFrames()), phase: "listening", started: time.Now(), asrTrace: &taskTrace{}}
		s.current = t
		t.inputRun = s.running
		// Sole writer: old media is invalid before this ready boundary. Queued
		// old PCM callbacks cannot write after it because they fail turn checks.
		if err := s.write(map[string]any{"type": "listen.ready", "requestId": msg.RequestID, "turn": t.number}); err != nil {
			return err
		}
		if s.running != nil && s.running.interruptState.Load() == interruptFailed {
			return s.reportInterruptFailure()
		}
		input, chatID := t.audio, s.chatID
		finishUsage, usageErr := s.beginUsage(chatID, !s.chatCreated)
		if usageErr != nil {
			return s.failTurn(t, "usage_failed", "语音用量存储不可用，请查看聊天界面")
		}
		started := s.service.start(func() {
			text, usages, err := s.asr.Recognize(withTaskTrace(ctx, t.asrTrace), input, func(text string, stopping bool) {
				ack := make(chan error, 1)
				if s.post(ctx, sessionEvent{kind: "partial", turn: t, text: text, stopping: stopping, ack: ack}) {
					select {
					case <-ack:
					case <-ctx.Done():
					}
				}
			}, func(samples, bytes int) { t.queuedSamples.Add(-int64(samples)); t.queuedBytes.Add(-int64(bytes)) })
			finalized := time.Now()
			stage := "asrFinal"
			if err != nil {
				stage = "asrFailed"
			}
			fields := t.asrTrace.fields(t.number)
			fields["cancelled"], fields["timedOut"] = ctx.Err() != nil, errors.Is(err, context.DeadlineExceeded)
			fields["runId"], fields["awaitingId"] = t.runID, t.awaitingID
			if t.inputRun != nil {
				if ref := t.inputRun.ref.Load(); ref != nil {
					fields["runId"] = ref.RunID
				}
			}
			s.logProviderUsage(t.number, usages)
			var writeErr error
			if finishUsage != nil {
				writeErr = s.saveUsage(chatID, "", usages)
				finishUsage(writeErr)
			}
			s.timing(stage, time.Now(), fields)
			s.post(ctx, sessionEvent{kind: "recognized", turn: t, text: text, err: err, usages: usages, usageErr: writeErr, finalized: finalized})
		})
		if !started && finishUsage != nil {
			finishUsage(nil)
		}
		return nil
	default:
		return s.fail("invalid_request", "不支持的语音控制消息")
	}
}

func (s *session) audio(pcm []byte) error {
	t := s.current
	if t == nil || t.audio == nil || t.phase != "listening" {
		return nil
	}
	format := s.asr.input
	samples, err := format.samples(pcm)
	if t.tailSeen || err != nil {
		s.invalidate()
		return s.fail("invalid_audio", "音频帧不符合协商格式")
	}
	t.tailSeen = format.Codec == "pcm_s16le" && len(pcm) < 640
	queuedSamples := t.queuedSamples.Add(int64(samples))
	queuedBytes := t.queuedBytes.Add(int64(len(pcm)))
	if queuedSamples <= int64(maxStartupAudio/time.Millisecond)*48 && queuedBytes <= int64(maxStartupAudioBytes) {
		select {
		case t.audio <- pcm:
			t.inputBytes += len(pcm)
			t.inputSamples += samples
			return nil
		default:
		}
	}
	t.queuedSamples.Add(-int64(samples))
	t.queuedBytes.Add(-int64(len(pcm)))
	// The queue covers provider startup, so name the stalled stage.
	observability.Log("voice", map[string]any{"sessionId": s.id, "turn": t.number, "stage": "asr_overflow", "asrPhase": t.asrTrace.phase(), "elapsedMs": time.Since(t.started).Milliseconds(), "audioMs": t.inputSamples / 48, "queuedMs": t.queuedSamples.Load() / 48, "queuedBytes": t.queuedBytes.Load(), "codec": format.Codec})
	s.invalidate()
	return s.fail("audio_overflow", "语音输入缓冲已满")
}

func (s *session) interrupt(run *ownedRun) {
	ref := run.ref.Load()
	if ref == nil || !run.cancelled.Load() {
		return
	}
	if !run.interruptState.CompareAndSwap(interruptIdle, interruptPending) &&
		!run.interruptState.CompareAndSwap(interruptFailed, interruptPending) {
		return
	}
	work := func() {
		ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
		defer cancel()
		source, reason, detail := contracts.InterruptSourceWSAPI, contracts.InterruptReasonUserCancelled, "voice abort"
		if s.service.ctx.Err() != nil {
			source, reason, detail = contracts.InterruptSourceServerSetup, contracts.InterruptReasonRunInterrupted, "voice shutdown"
		}
		result, err := s.service.deps.Runtime.Interrupt(ctx, rt.InterruptCommand{RunRef: *ref, Source: source, Reason: reason, Detail: detail})
		accepted := err == nil && result.Accepted
		if err == nil && !accepted {
			active, found, lookupErr := s.service.deps.Runs.ActiveRunForChat(ref.ChatID)
			accepted = lookupErr == nil && (!found || active.RunID != ref.RunID)
		}
		if accepted {
			run.interruptState.Store(interruptAccepted)
			return
		}
		run.interruptState.Store(interruptFailed)
		s.post(s.ctx, sessionEvent{kind: "interruptFailed", run: run})
	}
	if !s.service.start(work) {
		work()
	}
}

func (s *session) reportInterruptFailure() error {
	if s.current == nil || s.current.phase == "done" {
		return nil // Retain the failure on ownedRun until the next listen.
	}
	return s.failTurn(s.current, "run_failed", "任务中断失败，请再次点击打断或在聊天界面停止任务")
}

func (s *session) submit(t *turn) error {
	s.clearCompleted() // New input supersedes any unplayed result.
	primary := s.primary
	if primary {
		summary, err := s.service.deps.Chats.Summary(s.chatID)
		if err != nil {
			return s.failTurn(t, "run_failed", "聊天状态不可用")
		}
		if summary != nil {
			primary = false
			s.primary = false
		}
	}
	if primary && s.primaryIfMissing {
		current, err := s.service.deps.Chats.PrimaryChatID(s.agentKey)
		if err != nil || current != "" && current != s.chatID {
			return s.failTurn(t, "chat_changed", "主要聊天已变化，请重新连接语音")
		}
	}
	planning := false
	input := map[string]any{"agentKey": s.agentKey, "chatId": s.chatID, "message": t.text, "planningMode": false}
	if primary {
		input["primary"] = true
	}
	original, _ := json.Marshal(input)
	cmd := rt.QueryCommand{RequestID: s.operationID(t), AgentKey: s.agentKey, ChatID: s.chatID, Message: t.text, PlanningMode: &planning, Primary: primary, PrimaryIfMissing: primary && s.primaryIfMissing, OriginalQuery: original, Identity: s.caller.Identity, Locale: s.caller.Locale, ResourceBaseURL: s.caller.ResourceBaseURL}
	if s.caller.Identity != nil {
		cmd.Caller = rt.Caller{Subject: s.caller.Identity.Subject, DeviceID: s.caller.Identity.DeviceID, Scope: s.caller.Identity.Scope}
	}
	run := &ownedRun{}
	s.running = run
	t.phase = "running"
	t.text = ""
	runCtx := controlscope.WithContext(s.service.ctx, controlscope.Scope{Transport: "voice", Subject: cmd.Caller.Subject})
	s.service.start(func() {
		shutdownDone := make(chan struct{})
		stopShutdown := context.AfterFunc(s.service.ctx, func() { run.cancelled.Store(true); s.interrupt(run); close(shutdownDone) })
		defer func() {
			if !stopShutdown() {
				<-shutdownDone
			}
		}()
		var finishUsage func(error)
		var usageErr error
		defer func() {
			if finishUsage != nil {
				finishUsage(usageErr)
			}
		}()
		var observed <-chan struct{}
		observationCtx, stopObservation := context.WithCancel(s.ctx)
		defer stopObservation()
		result, err := s.service.deps.Runtime.ExecuteQueryWithHooks(runCtx, cmd, rt.QueryHooks{OnRunStarted: func(start chat.RunStart) {
			finishUsage, usageErr = s.beginUsage(start.ChatID, false)
			if usageErr == nil {
				usageErr = s.saveUsage(start.ChatID, start.RunID, t.usages)
			}
			if usageErr != nil {
				observability.Log("voice.usage.write_failed", map[string]any{"chatId": start.ChatID, "runId": start.RunID, "usages": t.usages})
			}
			ref := &rt.RunRef{RunID: start.RunID, ChatID: start.ChatID, AgentKey: start.AgentKey, Caller: cmd.Caller}
			s.timing("runStart", time.Now(), map[string]any{"turn": t.number, "runId": ref.RunID})
			run.ref.Store(ref)
			s.interrupt(run)
			if !s.post(s.ctx, sessionEvent{kind: "runStarted", run: run, turn: t}) {
				return
			}
			sub, attachErr := s.service.deps.Runtime.AttachRun(runCtx, *ref, 0)
			if attachErr != nil {
				s.post(s.ctx, sessionEvent{kind: "observationFailed", turn: t, run: run})
				return
			}
			done := make(chan struct{})
			observed = done
			started := s.service.start(func() {
				defer close(done)
				defer sub.Close()
				for {
					select {
					case <-observationCtx.Done():
						return
					case event, ok := <-sub.Events:
						if !ok {
							return
						}
						if event.Type == "request.steer" {
							s.timing("steerConsumedObserved", time.Now(), map[string]any{"runId": ref.RunID, "steerId": event.String("steerId")})
						}
						if event.Type == "awaiting.ask" || event.Type == "awaiting.answer" {
							if !s.post(observationCtx, sessionEvent{kind: event.Type, run: run, turn: t, awaitingID: event.String("awaitingId"), mode: event.String("mode")}) {
								return
							}
						}
					}
				}
			})
			if !started {
				sub.Close()
				close(done)
			}
		}})
		if result.Persisted && result.Completion != nil {
			s.timing("runEnd", result.PersistedAt, map[string]any{"turn": t.number, "runId": result.RunID, "finishReason": result.FinishReason, "success": err == nil && result.FinishReason == "complete"})
		} else {
			s.timing("runFailed", time.Now(), map[string]any{"turn": t.number, "runId": result.RunID})
		}
		// One deadline bounds both subscription drain and Chat release.
		deadline := time.NewTimer(finishTimeout)
		defer deadline.Stop()
		if observed != nil {
			select {
			case <-observed:
			case <-s.ctx.Done():
				stopObservation()
			case <-deadline.C:
				stopObservation()
				<-observed
				s.timing("runReleaseFailed", time.Now(), map[string]any{"turn": t.number, "runId": result.RunID, "reason": "observer_timeout"})
				s.post(s.ctx, sessionEvent{kind: "releaseFailed", turn: t, run: run})
				return
			}
		}
		// A cancel acknowledgment alone never releases Chat admission.
		poll := time.NewTicker(10 * time.Millisecond)
		defer poll.Stop()
		for {
			active, found, activeErr := s.service.deps.Runs.ActiveRunForChat(s.chatID)
			ref := run.ref.Load()
			if activeErr == nil && (!found || ref == nil || active.RunID != ref.RunID) {
				break
			}
			select {
			case <-s.service.ctx.Done():
				return
			case <-deadline.C:
				s.timing("runReleaseFailed", time.Now(), map[string]any{"turn": t.number, "runId": result.RunID, "reason": "chat_release_timeout"})
				s.post(s.ctx, sessionEvent{kind: "releaseFailed", turn: t, run: run})
				return
			case <-poll.C:
			}
		}
		s.timing("runReleased", time.Now(), map[string]any{"turn": t.number, "runId": result.RunID})
		done := make(chan struct{}, 1)
		if s.post(s.ctx, sessionEvent{kind: "runResult", run: run, turn: t, result: result, err: err, usageErr: usageErr, usageDone: done}) {
			select {
			case <-done:
			case <-s.ctx.Done():
			}
		}
	})
	return nil
}

func (s *session) event(e sessionEvent) error {
	retained := false
	defer func() {
		if !retained {
			acknowledgeUsage(&e)
		}
	}()
	if e.usageErr != nil {
		if err := s.fail("usage_failed", "语音用量写入失败，费用可能不完整，请查看聊天界面"); err != nil {
			return err
		}
		// Stop this media connection; accepted Runtime work remains independent.
		return errors.New("voice usage persistence failed")
	}
	if e.kind == "interruptFailed" {
		if e.run == s.running && e.run.interruptState.Load() == interruptFailed {
			return s.reportInterruptFailure()
		}
		return nil
	}
	if e.kind == "releaseFailed" {
		if e.run != s.running {
			return nil
		}
		if err := s.fail("run_busy_timeout", "任务收尾超时，请重新连接语音"); err != nil {
			return err
		}
		return errors.New("voice Run release timeout")
	}
	if (e.kind == "runStarted" || e.kind == "runResult" && e.result.RunID != "") && e.run == s.running {
		s.primary = false
		s.chatCreated = true
	}
	valid := e.turn != nil && e.turn == s.current && e.turn.ctx.Err() == nil
	if e.ack != nil {
		defer func() { e.ack <- nil }()
	}
	// Runtime observations belong to the owned Run, never to its first media
	// turn. Media callbacks below still require the current live turn.
	switch e.kind {
	case "runStarted", "observationFailed", "awaiting.ask", "awaiting.answer", "runResult":
		if e.run == nil || e.run != s.running {
			return nil
		}
		t := s.current
		mediaValid := t != nil && t.ctx.Err() == nil && !e.run.cancelled.Load()
		switch e.kind {
		case "observationFailed":
			if err := s.fail("run_failed", "无法观察当前任务，请查看聊天界面"); err != nil {
				return err
			}
			return errors.New("voice Run observation unavailable")
		case "runStarted":
			if ref := e.run.ref.Load(); ref != nil {
				if err := s.write(map[string]any{"type": "run", "state": "start", "runId": ref.RunID, "chatId": ref.ChatID}); err != nil {
					return err
				}
				if s.pending == t && mediaValid && !t.steered {
					s.clearPending()
					return s.routeInput(t)
				}
			}
		case "awaiting.ask":
			ref := e.run.ref.Load()
			if ref == nil {
				return nil
			}
			if e.mode == "question" {
				if mediaValid && t.intent == "question" && t.awaitingID != e.awaitingID {
					if err := s.resolveQuestionMedia(t.runID, t.awaitingID); err != nil {
						return err
					}
				}
				canSpeak := mediaValid && t.phase != "listening" && t.phase != "steering" && t.phase != "waiting" && !t.awaiting
				return s.presentQuestion(t, ref.RunID, e.awaitingID, canSpeak, false)
			}
			if err := s.write(map[string]any{"type": "awaiting", "state": "ask", "runId": ref.RunID, "awaitingId": e.awaitingID, "mode": e.mode}); err != nil {
				return err
			}
			// Do not interrupt input or reinterpret it as an answer to a new ask.
			if mediaValid && t.phase != "listening" && t.phase != "steering" && t.phase != "waiting" && !t.awaiting {
				t.awaiting = true
				t.phase = "awaiting"
				return s.speak(t, "需要你在界面上确认后继续。", "notice", ref.RunID, e.awaitingID)
			}
		case "awaiting.answer":
			if ref := e.run.ref.Load(); ref != nil {
				return s.resolveQuestionMedia(ref.RunID, e.awaitingID)
			}
			if mediaValid {
				t.awaiting = false
			}
		case "runResult":
			s.running = nil
			if e.result.RunID != "" {
				if err := s.write(map[string]any{"type": "run", "state": "end", "runId": e.result.RunID, "chatId": s.chatID, "finishReason": e.result.FinishReason}); err != nil {
					return err
				}
			}
			if s.pending != nil {
				pending := s.pending
				s.clearPending()
				if pending == t && pending.ctx.Err() == nil {
					return s.routeInput(pending)
				}
			}
			if mediaValid {
				if t.phase == "listening" || t.phase == "steering" {
					s.completed = &e
					retained = true
					return nil
				}
				return s.finishRun(t, e.result, e.err)
			}
		}
		return nil
	}
	if !valid {
		return nil
	}
	t := e.turn
	switch e.kind {
	case "partial":
		t.heard = t.heard || strings.TrimSpace(e.text) != ""
		if e.stopping {
			s.stopListening(t)
		}
		return s.write(map[string]any{"type": "stt", "turn": t.number, "text": e.text, "final": false})
	case "recognized":
		t.usages = e.usages
		if t.phase != "listening" {
			return nil // A finalized input can own only one operation.
		}
		s.stopListening(t)
		// Either 60s bound can win: buffered PCM can hit its byte limit before
		// the wall-clock deadline. Both end a silent turn without a Query.
		atRecordingLimit := errors.Is(e.err, context.DeadlineExceeded) || errors.Is(e.err, errRecordingLimit)
		if atRecordingLimit && !t.heard && t.inputBytes > 0 {
			e.err = nil
		}
		entry := map[string]any{"sessionId": s.id, "turn": t.number, "stage": "asr_end", "elapsedMs": time.Since(t.started).Milliseconds(), "audioMs": t.inputSamples / 48, "audioBytes": t.inputBytes, "codec": s.asr.input.Codec, "heard": t.heard, "failed": e.err != nil, "asrPhase": t.asrTrace.phase()}
		if started := t.asrTrace.started.Load(); started != nil {
			entry["startMs"] = started.Sub(t.started).Milliseconds()
		}
		if e.err != nil {
			entry["error"] = e.err.Error()
		}
		observability.Log("voice", entry)
		if e.err != nil {
			code := "asr_failed"
			for text, stable := range map[string]string{"recording limit": "recording_limit", "transcript limit": "transcript_limit", "startup audio": "audio_overflow"} {
				if strings.Contains(e.err.Error(), text) {
					code = stable
				}
			}
			return s.failTurn(t, code, "语音识别未完成，请重试")
		}
		if strings.TrimSpace(e.text) == "" {
			t.phase = "done"
			if err := s.write(map[string]any{"type": "stt", "turn": t.number, "text": "", "final": true, "pendingResult": s.completed != nil}); err != nil {
				return err
			}
			if completed := s.completed; completed != nil {
				return s.finishStoredRun(t, completed)
			}
			return nil
		}
		if t.intent == "question" {
			if _, err := s.questionAwaiting(t.runID, t.awaitingID); err != nil {
				return s.resolveQuestionMedia(t.runID, t.awaitingID)
			}
		}
		t.text = e.text
		t.phase = "running"
		if err := s.write(map[string]any{"type": "stt", "turn": t.number, "text": e.text, "final": true}); err != nil {
			return err
		}
		if t.intent == "question" {
			return s.answerQuestion(t, e.finalized)
		}
		return s.routeInput(t)
	case "questionResult":
		return s.finishQuestion(e)
	case "steerResult":
		return s.finishSteer(t, e.steer, e.err)
	case "pcm":
		if t.speech != e.speech || e.speech.ctx.Err() != nil {
			return nil
		}
		_ = s.conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := s.conn.WriteMessage(websocket.BinaryMessage, e.pcm); err != nil {
			return err
		}
		if e.speech.pcmBytes == 0 {
			fields := e.speech.trace.fields(t.number)
			fields["runId"], fields["awaitingId"], fields["speechId"], fields["purpose"] = e.speech.runID, e.speech.awaitingID, e.speech.id, e.speech.purpose
			s.timing("audioReleased", time.Now(), fields)
		}
		e.speech.pcmBytes += len(e.pcm)
		return nil
	case "spoken":
		if t.speech != e.speech || e.speech.ctx.Err() != nil {
			return nil
		}
		reason := "done"
		if e.err != nil {
			reason = "error"
		}
		entry := map[string]any{"sessionId": s.id, "turn": t.number, "stage": "tts_end", "reason": reason, "elapsedMs": time.Since(e.speech.started).Milliseconds(), "audioMs": e.speech.pcmBytes * 1000 / (s.sampleRate * 2), "textChars": e.speech.textChars}
		entry["runId"], entry["awaitingId"], entry["speechId"], entry["purpose"] = e.speech.runID, e.speech.awaitingID, e.speech.id, e.speech.purpose
		for key, value := range e.speech.trace.fields(t.number) {
			entry[key] = value
		}
		if e.err != nil {
			entry["error"] = e.err.Error()
		}
		observability.Log("voice", entry)
		if err := s.write(map[string]any{"type": "tts", "turn": t.number, "speechId": e.speech.id, "purpose": e.speech.purpose, "state": "stop", "reason": reason}); err != nil {
			return err
		}
		e.speech.cancel()
		if !e.speech.hint {
			t.phase = "done"
		}
		// An error ends the client's turn. The awaiting hint is advisory and its
		// Run is still waiting, so only stop(reason=error) reports it.
		if e.err != nil && !e.speech.hint {
			return s.fail("tts_failed", "语音播放未完成，请查看聊天界面")
		}
	}
	return nil
}

// finishRun is the only final-body playback gate. Even a retained result must
// be checked against current Chat facts after input has finished.
func (s *session) finishRun(t *turn, result rt.QueryResult, resultErr error) error {
	if resultErr != nil || !result.Persisted || result.Completion == nil || result.FinishReason != "complete" {
		code := "run_failed"
		var request *rt.RequestError
		if errors.As(resultErr, &request) {
			switch request.Code {
			case "active_run_conflict", "primary_chat_busy", "awaiting_pending":
				code = "chat_busy"
			case "chat_changed":
				code = "chat_changed"
			}
		}
		return s.failTurn(t, code, "当前问题未完成，请查看聊天界面")
	}
	summary, err := s.service.deps.Chats.Summary(s.chatID)
	if err != nil {
		return s.failTurn(t, "run_failed", "无法核对聊天完成状态")
	}
	if summary != nil && summary.PendingAwaiting != nil {
		return s.failTurn(t, "awaiting_pending", "请在聊天界面继续处理待确认项")
	}
	_, active, err := s.service.deps.Runs.ActiveRunForChat(s.chatID)
	if err != nil || active {
		return s.failTurn(t, "chat_busy", "聊天状态已变化，请查看聊天界面")
	}
	return s.speak(t, speechText(result.Completion.AssistantText), "final", result.RunID, "")
}

func (s *session) protocolError(message string) error {
	if err := s.fail("invalid_request", message); err != nil {
		return err
	}
	return errors.New("invalid voice control")
}

// Input bindings are checked against Runtime facts, not an event-derived
// replica of the waiting state. Only this connection's Run may be controlled.
func (s *session) validateInput(msg clientMessage) error {
	if msg.Intent == "question" {
		awaiting, err := s.questionAwaiting(msg.RunID, msg.AwaitingID)
		if err != nil {
			return errors.New("当前问题已失效，请查看聊天界面")
		}
		if q, _ := nextVoiceQuestion(awaiting); q != nil && q["id"] == msg.QuestionID {
			return nil
		}
		return errors.New("问题不存在或需要在界面填写")
	}
	if msg.Intent != "conversation" {
		return errors.New("收音意图无效")
	}
	summary, err := s.service.deps.Chats.Summary(s.chatID)
	if err != nil {
		return errors.New("无法核对聊天状态")
	}
	if summary != nil && summary.PendingAwaiting != nil {
		return errors.New("请在聊天界面处理当前等待项")
	}
	_, err = s.inputRunActive(s.running)
	return err
}
