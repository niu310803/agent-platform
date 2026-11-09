package voice

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"time"
	"unicode/utf8"
)

func (s *session) speak(t *turn, text string, purpose string, runID, awaitingID string) error {
	if t.speech != nil {
		t.speech.cancel()
	}
	ctx, cancel := context.WithCancel(t.ctx)
	hint := purpose != "final"
	media := &speechTask{id: uuid.NewString(), purpose: purpose, ctx: ctx, cancel: cancel, hint: hint, started: time.Now(), textChars: utf8.RuneCountInString(text)}
	media.runID, media.awaitingID, media.trace = runID, awaitingID, &taskTrace{}
	t.speech = media
	if !hint {
		t.phase = "speaking"
	}
	chatID := s.chatID
	finishUsage, usageErr := s.beginUsage(chatID, false)
	if usageErr != nil {
		cancel()
		return s.failTurn(t, "usage_failed", "语音用量存储不可用，请查看聊天界面")
	}
	if err := s.write(map[string]any{"type": "tts", "turn": t.number, "speechId": media.id, "purpose": purpose, "state": "start", "text": text}); err != nil {
		cancel()
		finishUsage(nil)
		return err
	}
	started := s.service.start(func() {
		frameBytes := s.sampleRate * 2 / 50
		var tail []byte
		var first time.Time
		sent := 0
		received := 0
		emit := func(frame []byte) error {
			if first.IsZero() {
				first = time.Now()
			}
			sent += len(frame)
			target := first.Add(time.Duration(sent)*time.Second/time.Duration(s.sampleRate*2) - time.Duration(s.service.deps.Config.DownlinkLeadMS)*time.Millisecond)
			if delay := time.Until(target); delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
			ack := make(chan error, 1)
			if !s.post(ctx, sessionEvent{kind: "pcm", turn: t, speech: media, pcm: append([]byte(nil), frame...), ack: ack}) {
				return context.Canceled
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-ack:
				return err
			}
		}
		usages, err := s.tts.Synthesize(withTaskTrace(ctx, media.trace), text, s.voice, s.sampleRate, func(pcm []byte) error {
			if received < 2 && received+len(pcm) >= 2 {
				fields := media.trace.fields(t.number)
				fields["runId"], fields["awaitingId"], fields["speechId"], fields["purpose"] = runID, awaitingID, media.id, purpose
				s.timing("ttsFirstAudio", time.Now(), fields)
			}
			received += len(pcm)
			for len(pcm) > 0 {
				n := min(frameBytes-len(tail), len(pcm))
				tail = append(tail, pcm[:n]...)
				pcm = pcm[n:]
				if len(tail) == frameBytes {
					if err := emit(tail); err != nil {
						return err
					}
					tail = tail[:0]
				}
			}
			return nil
		})
		s.logProviderUsage(t.number, usages)
		writeErr := s.saveUsage(chatID, runID, usages)
		finishUsage(writeErr)
		if err == nil && len(tail)%2 != 0 {
			err = errors.New("unaligned PCM tail")
		}
		if err == nil && len(tail) > 0 {
			err = emit(tail)
		}
		fields := media.trace.fields(t.number)
		fields["runId"], fields["awaitingId"], fields["speechId"], fields["purpose"] = runID, awaitingID, media.id, purpose
		fields["failed"], fields["cancelled"] = err != nil, ctx.Err() != nil
		s.timing("ttsFinished", time.Now(), fields) // Retained even when this media turn was superseded.
		s.post(ctx, sessionEvent{kind: "spoken", turn: t, speech: media, err: err, usageErr: writeErr})
	})
	if !started {
		finishUsage(nil)
		cancel()
	}
	return nil
}
