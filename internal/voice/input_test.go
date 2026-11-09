package voice

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agent-platform/internal/chat"
	"agent-platform/internal/contracts"
	"agent-platform/internal/runtime/controlscope"
	rt "agent-platform/internal/runtime/types"
)

type inputRuns struct {
	active atomic.Pointer[contracts.RunStatusInfo]
}

func (r *inputRuns) ActiveRunForChat(string) (contracts.RunStatusInfo, bool, error) {
	if active := r.active.Load(); active != nil {
		return *active, true, nil
	}
	return contracts.RunStatusInfo{}, false, nil
}

func nextInputEvent(t *testing.T, s *session) sessionEvent {
	t.Helper()
	select {
	case e := <-s.events:
		return e
	case <-time.After(time.Second):
		t.Fatal("missing input outcome")
		return sessionEvent{}
	}
}

func TestVoiceSteerRoutesOneFinalAndNeverRetriesUnknownOutcome(t *testing.T) {
	for _, outcome := range []string{"accepted", "unmatched", "unknown", "awaiting_pending", "unsupported"} {
		for _, completedFirst := range []bool{false, true} {
			t.Run(outcome+map[bool]string{true: "/completed_first", false: "/steer_first"}[completedFirst], func(t *testing.T) {
				runs := &inputRuns{}
				runs.active.Store(&contracts.RunStatusInfo{RunID: "owned"})
				calls, queries := make(chan rt.SteerCommand, 2), make(chan rt.QueryCommand, 2)
				reply := make(chan struct{})
				runtime := recoveryRuntime{
					status: func(string) (contracts.RunSnapshot, error) {
						return contracts.RunSnapshot{RunID: "owned", ChatID: "chat", AgentKey: "agent"}, nil
					},
					steer: func(ctx context.Context, cmd rt.SteerCommand) (rt.SteerResult, error) {
						if controlscope.FromContext(ctx).Transport != "voice" {
							t.Error("missing voice guard scope")
						}
						calls <- cmd
						select {
						case <-reply:
						case <-ctx.Done():
							return rt.SteerResult{}, ctx.Err()
						}
						if outcome == "unknown" {
							return rt.SteerResult{}, errors.New("unknown transport outcome")
						}
						return rt.SteerResult{Accepted: outcome == "accepted", Status: outcome}, nil
					},
					execute: func(_ context.Context, cmd rt.QueryCommand, _ rt.QueryHooks) (rt.QueryResult, error) {
						queries <- cmd
						return rt.QueryResult{}, errors.New("fixture ends before admission")
					},
				}
				s, client := eventSession(t, Dependencies{Runtime: runtime, Runs: runs, Chats: resultChats{summary: &chat.Summary{}}})
				s.id, s.agentKey = "session", "agent"
				run := &ownedRun{}
				run.ref.Store(&rt.RunRef{RunID: "owned", ChatID: "chat", AgentKey: "agent"})
				s.running, s.current = run, mediaTurn(t, 2)
				s.current.inputRun, s.current.requestID = run, "capture-2"
				final := sessionEvent{kind: "recognized", turn: s.current, text: "new instruction"}
				if err := s.event(final); err != nil {
					t.Fatal(err)
				}
				eventFrame(t, client)
				select {
				case cmd := <-calls:
					if cmd.RunID != "owned" || cmd.Message != final.text || cmd.SteerID != "voice:session:2" || cmd.RequestID != cmd.SteerID {
						t.Fatal(cmd)
					}
				case <-time.After(time.Second):
					t.Fatal("missing Steer")
				}
				if err := s.event(final); err != nil {
					t.Fatal(err)
				}
				complete := func() {
					runs.active.Store(nil)
					if err := s.event(sessionEvent{kind: "runResult", run: run, result: rt.QueryResult{RunID: "owned"}}); err != nil {
						t.Fatal(err)
					}
					if f := eventFrame(t, client); f["type"] != "run" {
						t.Fatal(f)
					}
				}
				if completedFirst {
					complete()
					if s.completed == nil || s.current.speech != nil {
						t.Fatal("result not held for Steer outcome")
					}
				}
				close(reply)
				if err := s.event(nextInputEvent(t, s)); err != nil {
					t.Fatal(err)
				}
				if outcome == "accepted" {
					if f := eventFrame(t, client); f["type"] != "steer" || f["state"] != "accepted" || f["requestId"] != "capture-2" {
						t.Fatal(f)
					}
					if completedFirst {
						eventFrame(t, client)
					} // Fixture's unpersisted final is refused.
				} else if outcome != "unmatched" {
					f := eventFrame(t, client)
					if f["type"] != "error" || outcome == "unknown" && f["code"] != "steer_outcome_unknown" {
						t.Fatal(f)
					}
				}
				if !completedFirst {
					if outcome == "unmatched" && s.pending != s.current {
						t.Fatal("lost tail input")
					}
					complete()
					if outcome == "accepted" {
						eventFrame(t, client)
					}
				}
				if outcome == "unmatched" {
					select {
					case cmd := <-queries:
						if cmd.Message != final.text || cmd.RequestID != "voice:session:2" {
							t.Fatal(cmd)
						}
					case <-time.After(time.Second):
						t.Fatal("explicit non-admission did not become next Query")
					}
				}
				s.service.Close()
				if len(queries) != 0 || len(calls) != 0 || s.completed != nil || s.pending != nil {
					t.Fatal("duplicated or retained resolved operation")
				}
			})
		}
	}
}

func TestVoiceFinalRechecksFrozenRunAndAwaiting(t *testing.T) {
	for _, changed := range []string{"other_run", "new_awaiting", "runtime_awaiting", "old_run_released_new_run", "no_binding"} {
		t.Run(changed, func(t *testing.T) {
			runs := &inputRuns{}
			runs.active.Store(&contracts.RunStatusInfo{RunID: "owned"})
			summary := &chat.Summary{}
			snapshot := contracts.RunSnapshot{RunID: "owned", ChatID: "chat", AgentKey: "agent"}
			runtime := recoveryRuntime{status: func(string) (contracts.RunSnapshot, error) { return snapshot, nil }, steer: func(context.Context, rt.SteerCommand) (rt.SteerResult, error) {
				t.Error("steered changed input")
				return rt.SteerResult{}, nil
			}, execute: func(context.Context, rt.QueryCommand, rt.QueryHooks) (rt.QueryResult, error) {
				t.Error("queried changed input")
				return rt.QueryResult{}, nil
			}}
			s, client := eventSession(t, Dependencies{Runtime: runtime, Runs: runs, Chats: resultChats{summary: summary}})
			s.agentKey = "agent"
			run := &ownedRun{}
			run.ref.Store(&rt.RunRef{RunID: "owned"})
			s.running, s.current = run, mediaTurn(t, 2)
			s.current.inputRun = run
			switch changed {
			case "other_run":
				runs.active.Store(&contracts.RunStatusInfo{RunID: "foreign"})
			case "new_awaiting":
				summary.PendingAwaiting = &chat.PendingAwaiting{AwaitingID: "new"}
			case "runtime_awaiting":
				snapshot.AwaitingCount = 1
			case "old_run_released_new_run":
				s.running = nil
				runs.active.Store(&contracts.RunStatusInfo{RunID: "foreign"})
			case "no_binding":
				s.current.inputRun = nil
			}
			if err := s.event(sessionEvent{kind: "recognized", turn: s.current, text: "do it"}); err != nil {
				t.Fatal(err)
			}
			eventFrame(t, client)
			if f := eventFrame(t, client); f["code"] != "input_changed" {
				t.Fatal(f)
			}
			if s.pending != nil {
				t.Fatal("queued invalid input")
			}
		})
	}
}

func TestVoiceTailInputRechecksAfterRunRelease(t *testing.T) {
	for _, changed := range []string{"new_run", "awaiting", "new_media"} {
		t.Run(changed, func(t *testing.T) {
			runs := &inputRuns{}
			summary := &chat.Summary{}
			s, client := eventSession(t, Dependencies{Runs: runs, Chats: resultChats{summary: summary}})
			run := &ownedRun{}
			run.ref.Store(&rt.RunRef{RunID: "owned"})
			s.running, s.current = run, mediaTurn(t, 2)
			s.current.inputRun, s.current.text, s.current.steered = run, "tail input", true
			if err := s.waitForRun(s.current); err != nil {
				t.Fatal(err)
			}
			switch changed {
			case "new_run":
				runs.active.Store(&contracts.RunStatusInfo{RunID: "foreign"})
			case "awaiting":
				summary.PendingAwaiting = &chat.PendingAwaiting{AwaitingID: "new"}
			case "new_media":
				s.invalidate()
				s.clearPending()
				s.current = mediaTurn(t, 3)
			}
			if err := s.event(sessionEvent{kind: "runResult", run: run, result: rt.QueryResult{RunID: "owned"}}); err != nil {
				t.Fatal(err)
			}
			eventFrame(t, client)
			if changed != "new_media" {
				if f := eventFrame(t, client); f["code"] != "input_changed" {
					t.Fatal(f)
				}
			}
			if s.pending != nil || s.running != nil {
				t.Fatal("stale final retained or submitted")
			}
		})
	}
}

func TestVoiceInputWaitsForOwnedRunIdentityThenSteersOnce(t *testing.T) {
	runs := &inputRuns{}
	runs.active.Store(&contracts.RunStatusInfo{RunID: "owned"}) // Registered before the hook reaches voice.
	calls := make(chan rt.SteerCommand, 2)
	runtime := recoveryRuntime{
		status: func(string) (contracts.RunSnapshot, error) {
			return contracts.RunSnapshot{ChatID: "chat", AgentKey: "agent"}, nil
		},
		steer: func(_ context.Context, cmd rt.SteerCommand) (rt.SteerResult, error) {
			calls <- cmd
			return rt.SteerResult{Accepted: true}, nil
		},
	}
	s, client := eventSession(t, Dependencies{Runtime: runtime, Runs: runs, Chats: resultChats{summary: &chat.Summary{}}})
	s.agentKey = "agent"
	run := &ownedRun{}
	s.running, s.current = run, mediaTurn(t, 2)
	s.current.inputRun = run
	if err := s.validateInput(clientMessage{Intent: "conversation"}); err != nil {
		t.Fatal("unpublished owned identity was treated as foreign", err)
	}
	if err := s.event(sessionEvent{kind: "recognized", turn: s.current, text: "while starting"}); err != nil {
		t.Fatal(err)
	}
	eventFrame(t, client)
	if s.pending != s.current {
		t.Fatal("startup final lost")
	}
	run.ref.Store(&rt.RunRef{RunID: "owned", ChatID: "chat", AgentKey: "agent"})
	runs.active.Store(&contracts.RunStatusInfo{RunID: "owned"})
	if err := s.event(sessionEvent{kind: "runStarted", run: run}); err != nil {
		t.Fatal(err)
	}
	eventFrame(t, client)
	if err := s.event(nextInputEvent(t, s)); err != nil {
		t.Fatal(err)
	}
	if f := eventFrame(t, client); f["type"] != "steer" {
		t.Fatal(f)
	}
	if cmd := <-calls; cmd.RunID != "owned" || cmd.Message != "while starting" {
		t.Fatal(cmd)
	}
	if s.pending != nil || s.waitTimer != nil || len(calls) != 0 {
		t.Fatal("startup wait was not released")
	}
}
