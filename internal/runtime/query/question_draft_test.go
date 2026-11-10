package query

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"agent-platform/internal/contracts"
	"agent-platform/internal/contracts/queryinput"
	"agent-platform/internal/runtime/controlscope"
	"agent-platform/internal/runtime/runstate"
	rt "agent-platform/internal/runtime/types"
)

func questionDraftFixture(t *testing.T, questions []any) (*Service, *contracts.RunControl, context.Context, rt.RunRef) {
	t.Helper()
	runs := runstate.NewManager()
	_, control, _ := runs.Register(context.Background(), contracts.QuerySession{RunID: "run", ChatID: "chat", AgentKey: "agent", RunOwner: contracts.AgentRunOwner("agent")})
	t.Cleanup(func() { control.Finish() })
	owner := controlscope.Scope{Transport: "voice", Subject: "owner"}
	runs.BindRunControlScope("run", owner)
	control.ExpectSubmit(contracts.AwaitingSubmitContext{AwaitingID: "ask", Mode: "question", Questions: questions, ItemCount: len(questions)})
	control.TransitionState(contracts.RunLoopStateWaitingSubmit)
	return NewService(Dependencies{Runs: runs}), control, controlscope.WithContext(context.Background(), owner), rt.RunRef{RunID: "run", ChatID: "chat", AgentKey: "agent", Caller: rt.Caller{Subject: "owner"}}
}

func TestQuestionSubmitGuardRechecksChangedDefinitionUnderAwaitingLock(t *testing.T) {
	for _, kind := range []string{"password", "approval", "form", "planning"} {
		t.Run(kind, func(t *testing.T) {
			s, control, ctx, ref := questionDraftFixture(t, []any{map[string]any{"id": "q", "type": "text"}})
			if _, err := s.UpdateQuestionDraft(ctx, rt.QuestionDraftCommand{RunRef: ref, AwaitingID: "ask", Answer: map[string]any{"id": "q", "answer": "voice"}}); err != nil {
				t.Fatal(err)
			}
			mode := kind
			if kind == "password" {
				mode = "question"
			}
			control.ExpectSubmit(contracts.AwaitingSubmitContext{AwaitingID: "ask", Mode: mode, Questions: []any{map[string]any{"id": "q", "type": kind}}})
			request := queryinput.SubmitRequest{RunID: "run", AwaitingID: "ask", SubmitID: "stable", RequireVoiceQuestion: true, Params: queryinput.SubmitParams{json.RawMessage(`{"id":"q","answer":"voice"}`)}}
			if ack := control.ResolveSubmit(request); ack.Accepted {
				t.Fatal("changed definition accepted", ack)
			}
			if _, ok := control.LookupAwaiting("ask"); !ok {
				t.Fatal("rejection consumed awaiting")
			}
			encoded, _ := json.Marshal(request)
			if strings.Contains(string(encoded), "RequireVoiceQuestion") {
				t.Fatal("internal guard entered public protocol")
			}
		})
	}
}

func TestQuestionDraftAllOrdinaryTypesRemainTemporary(t *testing.T) {
	questions := []any{
		map[string]any{"id": "text", "type": "text"}, map[string]any{"id": "number", "type": "number"},
		map[string]any{"id": "date", "type": "date"}, map[string]any{"id": "datetime", "type": "datetime"},
		map[string]any{"id": "select", "type": "select", "options": []any{map[string]any{"label": "one"}}},
		map[string]any{"id": "multi", "type": "multi-select", "allowFreeText": true, "options": []any{map[string]any{"label": "one"}}},
		map[string]any{"id": "password", "type": "password"},
	}
	s, control, ctx, ref := questionDraftFixture(t, questions)
	answers := []map[string]any{{"id": "text", "answer": "draft-only"}, {"id": "number", "answer": 12}, {"id": "date", "answer": "2028-02-29"}, {"id": "datetime", "answer": "2026-11-07 15:00:00"}, {"id": "select", "answer": "one"}, {"id": "multi", "answers": []any{"one", "free"}}}
	for i, answer := range answers {
		draft, err := s.UpdateQuestionDraft(ctx, rt.QuestionDraftCommand{RunRef: ref, AwaitingID: "ask", Answer: answer})
		if err != nil || draft.Revision != int64(i+1) || len(draft.Answers) != i+1 {
			t.Fatal(draft, err)
		}
		draft.Answers[0]["answer"] = "external mutation"
	}
	current, _ := control.LookupAwaiting("ask")
	if current.QuestionDraft.Answers[0]["answer"] != "draft-only" {
		t.Fatal("returned draft aliases locked state")
	}
	current.QuestionDraft.Answers[5]["answers"].([]string)[0] = "changed"
	current, _ = control.LookupAwaiting("ask")
	if current.QuestionDraft.Answers[5]["answers"].([]string)[0] != "one" {
		t.Fatal("multi-select clone aliases state")
	}
	control.ExpectSubmit(contracts.AwaitingSubmitContext{AwaitingID: "ask", Mode: "question", Questions: questions})
	current, _ = control.LookupAwaiting("ask")
	if current.QuestionDraft.Revision != 6 {
		t.Fatal("repeated awaiting delivery lost draft")
	}
	for _, answer := range []map[string]any{{"id": "password", "answer": "never-store"}, {"id": "unknown", "answer": "x"}, {"id": "text", "answer": "overwrite"}} {
		if _, err := s.UpdateQuestionDraft(ctx, rt.QuestionDraftCommand{RunRef: ref, AwaitingID: "ask", Answer: answer}); err == nil {
			t.Fatal("invalid draft accepted", answer["id"])
		}
	}
	snapshot, err := s.RunStatus("run")
	if err != nil || snapshot.Awaiting.QuestionDraft.Revision != 6 {
		t.Fatal(snapshot, err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(data), "draft-only") || strings.Contains(string(data), "questionDraft") {
		t.Fatal("draft entered public/model Run snapshot", string(data), err)
	}
	control.Finish()
	if _, ok := control.LookupAwaiting("ask"); ok {
		t.Fatal("terminal Run retained draft")
	}
}

func TestQuestionDraftRejectsIdentityAndResolvedRaces(t *testing.T) {
	q := []any{map[string]any{"id": "q", "type": "text"}}
	s, _, ctx, ref := questionDraftFixture(t, q)
	cmd := rt.QuestionDraftCommand{RunRef: ref, AwaitingID: "ask", Answer: map[string]any{"id": "q", "answer": "answer"}}
	for _, caller := range []controlscope.Scope{{Transport: "voice", Subject: "other"}, {Transport: "http", Subject: "owner"}, {Transport: "ws", Subject: "owner", Lane: "main"}} {
		if _, err := s.UpdateQuestionDraft(controlscope.WithContext(context.Background(), caller), cmd); err == nil {
			t.Fatal("draft write crossed voice owner", caller)
		}
	}
	bad := cmd
	bad.ChatID = "other"
	if _, err := s.UpdateQuestionDraft(ctx, bad); err == nil {
		t.Fatal("wrong Chat accepted")
	}
	for i := 0; i < 30; i++ {
		s, control, ctx, ref := questionDraftFixture(t, q)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.UpdateQuestionDraft(ctx, rt.QuestionDraftCommand{RunRef: ref, AwaitingID: "ask", Answer: cmd.Answer})
		}()
		go func() {
			defer wg.Done()
			control.ResolveSubmit(queryinput.SubmitRequest{RunID: "run", AwaitingID: "ask", SubmitID: "ui", Params: queryinput.SubmitParams{json.RawMessage(`{"answer":"UI"}`)}})
		}()
		wg.Wait()
		if _, ok := control.LookupAwaiting("ask"); ok {
			t.Fatal("draft resurrected submitted waiting item")
		}
		if _, err := s.UpdateQuestionDraft(ctx, rt.QuestionDraftCommand{RunRef: ref, AwaitingID: "ask", Answer: cmd.Answer}); err == nil {
			t.Fatal("late draft accepted")
		}
	}
}
