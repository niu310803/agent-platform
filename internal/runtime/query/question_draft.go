package query

import (
	"context"
	"strings"
	"time"

	"agent-platform/internal/contracts"
	"agent-platform/internal/contracts/queryinput"
	"agent-platform/internal/runtime/controlscope"
	rt "agent-platform/internal/runtime/types"
	"agent-platform/internal/toolinteraction"
)

func (s *Service) validateVoiceQuestionOwner(ctx context.Context, ref rt.RunRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.ValidateRunOwner(ref.RunID, ref.AgentKey); err != nil {
		return err
	}
	status, _ := s.deps.Runs.RunStatus(ref.RunID)
	if ref.ChatID == "" || status.ChatID != ref.ChatID {
		return &statusError{Status: 400, Message: "chatId does not match run"}
	}
	caller := controlscope.FromContext(ctx)
	owner, err := s.runControlScopes().Load(ref.RunID)
	if err != nil || caller.Transport != "voice" || owner.Transport != "voice" || caller.Subject != ref.Caller.Subject || controlscope.Check(owner, caller) != "" {
		return &statusError{Status: 403, Message: "voice question does not belong to caller"}
	}
	return nil
}

func (s *Service) UpdateQuestionDraft(ctx context.Context, command rt.QuestionDraftCommand) (*queryinput.QuestionDraft, error) {
	if err := s.validateVoiceQuestionOwner(ctx, command.RunRef); err != nil {
		return nil, err
	}
	updater, ok := s.deps.Runs.(contracts.QuestionDraftUpdater)
	if !ok {
		return nil, contracts.ErrQuestionDraftUnavailable
	}
	draft, err := updater.UpdateQuestionDraft(command.RunID, command.AwaitingID, command.Answer, toolinteraction.NormalizeQuestionSubmitItem)
	if err != nil {
		return nil, err
	}
	if s.deps.Notifications != nil {
		// Answers travel only through the authenticated Chat projection. This
		// ordinary notification is a refresh signal, not an event/LLM message.
		s.deps.Notifications.Broadcast("chat.awaiting", map[string]any{"chatId": command.ChatID, "agentKey": command.AgentKey, "runId": command.RunID, "awaitingId": command.AwaitingID, "mode": "question", "questionDraftChanged": true, "updatedAt": time.Now().UnixMilli()})
	}
	return draft, nil
}

func (s *Service) validateVoiceQuestionSubmit(ctx context.Context, command rt.SubmitCommand) error {
	if err := s.validateVoiceQuestionOwner(ctx, command.RunRef); err != nil {
		return err
	}
	awaiting, ok := s.deps.Runs.LookupAwaiting(command.RunID, command.AwaitingID)
	if !ok {
		// Let the existing same-submitId receipt decide a repeated outcome.
		if receipts, supported := s.deps.Runs.(interface {
			LookupResolvedSubmit(string, string) (contracts.SubmitAck, bool)
		}); supported {
			if receipt, found := receipts.LookupResolvedSubmit(command.RunID, command.AwaitingID); found && command.SubmitID != "" && receipt.SubmitID == command.SubmitID {
				return nil
			}
		}
		return contracts.ErrQuestionDraftUnavailable
	}
	if awaiting.Mode != "question" {
		return contracts.ErrQuestionDraftUnavailable
	}
	for _, raw := range awaiting.Questions {
		q, _ := raw.(map[string]any)
		if strings.EqualFold(contracts.AnyStringNode(q["type"]), "password") {
			return &statusError{Status: 400, Message: "password questions require UI submission"}
		}
	}
	return nil
}
