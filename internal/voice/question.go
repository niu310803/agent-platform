package voice

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"agent-platform/internal/contracts"
	"agent-platform/internal/runtime/controlscope"
	rt "agent-platform/internal/runtime/types"
	"github.com/google/uuid"
)

// Read the existing Runtime awaiting each time; media never owns a second
// question queue or a copy of the answers.
func (s *session) questionAwaiting(runID, awaitingID string) (*contracts.RunAwaiting, error) {
	if s.running == nil || s.running.cancelled.Load() {
		return nil, contracts.ErrQuestionDraftUnavailable
	}
	ref := s.running.ref.Load()
	if ref == nil || ref.RunID != runID {
		return nil, contracts.ErrQuestionDraftUnavailable
	}
	status, err := s.service.deps.Runtime.GetRunStatus(runID)
	if err != nil || status.ChatID != s.chatID || status.AgentKey != s.agentKey || status.AwaitingCount != 1 || status.Awaiting == nil || status.Awaiting.Mode != "question" || status.Awaiting.AwaitingID != awaitingID {
		return nil, contracts.ErrQuestionDraftUnavailable
	}
	return status.Awaiting, nil
}

func nextVoiceQuestion(awaiting *contracts.RunAwaiting) (map[string]any, bool) {
	answered := map[string]bool{}
	if awaiting.QuestionDraft != nil {
		for _, item := range awaiting.QuestionDraft.Answers {
			answered[contracts.AnyStringNode(item["id"])] = true
		}
	}
	var next map[string]any
	password := false
	for _, raw := range awaiting.Questions {
		q := contracts.AnyMapNode(raw)
		if strings.EqualFold(strings.TrimSpace(contracts.AnyStringNode(q["type"])), "password") {
			password = true
			continue
		}
		if next == nil && !answered[contracts.AnyStringNode(q["id"])] {
			next = q
		}
	}
	return next, password
}

func questionPrompt(question map[string]any) string {
	text := contracts.AnyStringNode(question["question"])
	if options, ok := question["options"].([]any); ok {
		for i, raw := range options {
			text += fmt.Sprintf("。第%d项，%s", i+1, contracts.AnyStringNode(contracts.AnyMapNode(raw)["label"]))
		}
	}
	return speechText(text)
}

func (s *session) presentQuestion(t *turn, runID, awaitingID string, speak, retry bool) error {
	awaiting, err := s.questionAwaiting(runID, awaitingID)
	if err != nil {
		return s.resolveQuestionMedia(runID, awaitingID)
	}
	question, _ := nextVoiceQuestion(awaiting)
	questionID := ""
	if question != nil {
		questionID = contracts.AnyStringNode(question["id"])
	}
	if err := s.write(map[string]any{"type": "awaiting", "state": "ask", "runId": runID, "awaitingId": awaitingID, "mode": "question", "questionId": questionID, "handoff": question == nil}); err != nil {
		return err
	}
	if t == nil || !speak || t.ctx.Err() != nil {
		return nil
	}
	t.awaiting, t.phase = true, "awaiting"
	if question == nil {
		return s.speak(t, "已填写的答案会保留，请在聊天界面补齐并提交。", "notice", runID, awaitingID)
	}
	text := questionPrompt(question)
	if retry {
		text = "答案不够明确，请再说一次。" + text
	}
	return s.speak(t, text, "question", runID, awaitingID)
}

func (s *session) resolveQuestionMedia(runID, awaitingID string) error {
	if t := s.current; t != nil {
		boundInput := t.intent == "question" && t.runID == runID && t.awaitingID == awaitingID
		boundSpeech := t.speech != nil && t.speech.runID == runID && t.speech.awaitingID == awaitingID
		if boundInput || boundSpeech {
			s.stopListening(t)
			if boundSpeech {
				t.speech.cancel()
				t.speech = nil
			}
			t.phase, t.awaiting, t.text = "running", false, ""
		}
	}
	return s.write(map[string]any{"type": "awaiting", "state": "resolved", "runId": runID, "awaitingId": awaitingID})
}

func (s *session) answerQuestion(t *turn, finalized time.Time) error {
	awaiting, err := s.questionAwaiting(t.runID, t.awaitingID)
	if err != nil {
		return s.resolveQuestionMedia(t.runID, t.awaitingID)
	}
	question, _ := nextVoiceQuestion(awaiting)
	if question == nil || question["id"] != t.questionID {
		return s.presentQuestion(t, t.runID, t.awaitingID, true, false)
	}
	answer, err := convertQuestionAnswer(question, t.text, s.timeZone, finalized)
	t.text = ""
	if err != nil {
		return s.presentQuestion(t, t.runID, t.awaitingID, true, true)
	}
	run, ref := s.running, *s.running.ref.Load()
	t.phase = "answering"
	// Capture immutable bindings; the worker touches no session media state.
	chatID, awaitingID, usages := s.chatID, t.awaitingID, t.usages
	s.service.start(func() {
		ctx, cancel := context.WithTimeout(t.ctx, writeTimeout)
		defer cancel()
		ctx = controlscope.WithContext(ctx, controlscope.Scope{Transport: "voice", Subject: ref.Caller.Subject})
		draft, err := s.service.deps.Runtime.UpdateQuestionDraft(ctx, rt.QuestionDraftCommand{RunRef: ref, AwaitingID: awaitingID, Answer: answer})
		var usageErr error
		var result rt.SubmitResult
		if err == nil {
			finish, beginErr := s.beginUsage(chatID, false)
			usageErr = beginErr
			if beginErr == nil {
				usageErr = s.saveUsage(chatID, ref.RunID, usages)
				finish(usageErr)
			}
			awaiting.QuestionDraft = draft
			next, password := nextVoiceQuestion(awaiting)
			if next == nil && !password && usageErr == nil {
				params := make([]json.RawMessage, 0, len(awaiting.Questions))
				for _, raw := range awaiting.Questions {
					for _, item := range draft.Answers {
						if item["id"] == contracts.AnyMapNode(raw)["id"] {
							data, marshalErr := json.Marshal(item)
							if marshalErr != nil {
								err = marshalErr
								break
							}
							params = append(params, data)
							break
						}
					}
				}
				if err == nil {
					id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(ref.RunID+"\x00"+awaitingID)).String()
					result, err = s.service.deps.Runtime.Submit(ctx, rt.SubmitCommand{RunRef: ref, AwaitingID: awaitingID, SubmitID: id, Params: params})
				}
			}
		}
		s.post(s.ctx, sessionEvent{kind: "questionResult", turn: t, run: run, awaitingID: awaitingID, submitResult: result, err: err, usageErr: usageErr})
	})
	return nil
}

func (s *session) finishQuestion(e sessionEvent) error {
	if e.run != s.running || e.turn.phase != "answering" {
		return nil
	}
	if _, err := s.questionAwaiting(e.turn.runID, e.awaitingID); err != nil {
		return s.resolveQuestionMedia(e.turn.runID, e.awaitingID)
	}
	if e.err != nil || e.submitResult.Status != "" && !e.submitResult.Accepted {
		// Unknown Submit outcomes are not retried with a new identity. The
		// current awaiting and its draft remain available in the original UI.
		return s.failTurn(e.turn, "question_failed", "答案提交未确认，请在聊天界面检查；不会自动重发")
	}
	return s.presentQuestion(e.turn, e.turn.runID, e.awaitingID, true, false)
}
