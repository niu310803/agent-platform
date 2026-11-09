package conversation

import (
	"errors"
	"sync"

	"agent-platform/internal/chat"
	"agent-platform/internal/observability"
)

var ErrVoiceUsagePending = errors.New("voice usage is still processing; retry archive/delete later")

func (s *Service) voiceOwner() *Service {
	for s.mutationOwner != nil {
		s = s.mutationOwner
	}
	return s
}

// BeginVoiceUsage protects only media work that may still return costs. An
// unknown provider quantity is a valid terminal result; a failed write retains
// the hold so archival cannot silently claim complete accounting.
func (s *Service) BeginVoiceUsage(chatID string) (func(error), error) {
	release := s.LockMutation()
	defer release()
	if !chat.ValidChatID(chatID) {
		return nil, chat.ErrChatNotFound
	}
	summary, err := s.Chats.Summary(chatID)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return nil, chat.ErrChatNotFound
	}
	owner := s.voiceOwner()
	if owner.voicePending == nil {
		owner.voicePending = map[string]int{}
	}
	owner.voicePending[chatID]++
	var once sync.Once
	return func(writeErr error) {
		once.Do(func() {
			release := s.LockMutation()
			defer release()
			if writeErr != nil {
				observability.Log("voice.usage.write_failed", map[string]any{"chatId": chatID, "pending": true})
				return
			}
			owner.voicePending[chatID]--
			if owner.voicePending[chatID] == 0 {
				delete(owner.voicePending, chatID)
			}
		})
	}, nil
}

func (s *Service) ensureNoVoiceUsage(chatID string) error {
	if s.voiceOwner().voicePending[chatID] > 0 {
		return ErrVoiceUsagePending
	}
	return nil
}

func (s *Service) AppendVoiceUsage(chatID string, usage chat.VoiceUsage) (bool, error) {
	release := s.LockMutation()
	if s.voiceOwner().voicePending[chatID] <= 0 {
		release()
		return false, errors.New("voice usage has no active accounting work")
	}
	store, ok := s.Chats.(interface {
		AppendVoiceUsage(string, chat.VoiceUsage) (bool, error)
	})
	if !ok {
		release()
		return false, ErrNotConfigured
	}
	changed, err := store.AppendVoiceUsage(chatID, usage)
	var summary *chat.Summary
	if err == nil && changed {
		summary, err = s.Chats.Summary(chatID)
		if err == nil && summary == nil {
			err = chat.ErrChatNotFound
		}
	}
	release()
	if err != nil {
		// Metadata-only recovery evidence survives even if JSONL writing failed.
		observability.Log("voice.usage.write_failed", map[string]any{"chatId": chatID, "usage": usage})
		return false, err
	}
	if changed && s.Notifications != nil {
		s.Notifications.Broadcast("chat.updated", map[string]any{"chatId": chatID, "agentKey": summary.AgentKey, "updatedAt": summary.UpdatedAt, "usageChanged": true})
	}
	return changed, nil
}
