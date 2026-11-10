package voice

import (
	"context"
	"errors"
	"math"

	"agent-platform/internal/chat"
	"agent-platform/internal/modelusage"
	"agent-platform/internal/observability"
)

// Provider payload quantities are cumulative. A null/missing usage is unknown,
// including on task-finished; it must not erase earlier reported quantities.
func recordProviderUsage(event dashScopeEvent, usage *chat.VoiceUsage) error {
	if usage == nil || event.Payload.Usage == nil {
		return nil
	}
	reported := event.Payload.Usage
	if usage.Operation == modelusage.OperationRecognition && reported.Duration != nil {
		value := *reported.Duration
		if value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return errors.New("invalid provider billed duration")
		}
		usage.BilledAudioSeconds = max(usage.BilledAudioSeconds, value)
		usage.Known = true
	}
	if usage.Operation == modelusage.OperationSpeech && reported.Characters != nil {
		if *reported.Characters < 0 {
			return errors.New("invalid provider billed characters")
		}
		usage.Characters = max(usage.Characters, *reported.Characters)
		usage.Known = true
	}
	return nil
}

// Called after the reader has stopped and joined, so cancellation cannot drop
// an already-read usage snapshot or race with terminal classification.
func finishProviderUsage(usages []chat.VoiceUsage, err error) {
	for i := range usages {
		if usages[i].Status != "running" {
			continue
		}
		usages[i].Status = "complete"
		if errors.Is(err, context.Canceled) {
			usages[i].Status = "cancelled"
		} else if err != nil {
			usages[i].Status = "failed"
		}
	}
}

func (s *session) logProviderUsage(turn int, usages []chat.VoiceUsage) {
	for _, usage := range usages {
		observability.Log("voice.usage", map[string]any{"sessionId": s.id, "turn": turn, "usage": usage})
	}
}

// A reserved new Chat has no accounting destination until Runtime creates it.
// Existing Chats are protected before any provider work begins.
func (s *session) beginUsage(chatID string, allowMissing bool) (func(error), error) {
	if s.service.deps.Usage == nil {
		return nil, errors.New("voice usage storage unavailable")
	}
	finish, err := s.service.deps.Usage.BeginVoiceUsage(chatID)
	if allowMissing && errors.Is(err, chat.ErrChatNotFound) {
		return nil, nil
	}
	return finish, err
}

func (s *session) saveUsage(chatID, runID string, usages []chat.VoiceUsage) error {
	var writeErr error
	for _, usage := range usages {
		usage.RunID = runID
		if _, err := s.service.deps.Usage.AppendVoiceUsage(chatID, usage); err != nil {
			writeErr = errors.Join(writeErr, err)
		}
	}
	return writeErr
}

// The query worker holds accounting until the loop either starts TTS (which
// acquires its own hold synchronously) or discards the result. A disconnected
// loop releases the worker through its context without cancelling the Run.
func acknowledgeUsage(e *sessionEvent) {
	if e != nil && e.usageDone != nil {
		select {
		case e.usageDone <- struct{}{}:
		default:
		}
	}
}

func (s *session) clearCompleted() {
	acknowledgeUsage(s.completed)
	s.completed = nil
}

func (s *session) finishStoredRun(t *turn, e *sessionEvent) error {
	s.completed = nil
	defer acknowledgeUsage(e)
	return s.finishRun(t, e.result, e.err)
}
