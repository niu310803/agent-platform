package voice

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"agent-platform/internal/chat"
	"agent-platform/internal/models"
	"github.com/gorilla/websocket"
)

type ASRClient struct {
	socket    *dashScopeSocket
	silenceMS int
	input     audioFormat
}

var errRecordingLimit = errors.New("voice recording limit exceeded")

func NewASRClient(model models.ModelDefinition, provider models.ProviderDefinition, silenceMS int) (*ASRClient, error) {
	if silenceMS < 200 || silenceMS > 6000 {
		return nil, errors.New("invalid voice sentence silence")
	}
	socket, err := newDashScopeSocket(model, provider, models.ModelTypeASR, models.ProtocolDashScopeASR)
	if err != nil {
		return nil, err
	}
	return &ASRClient{socket: socket, silenceMS: silenceMS, input: audioFormat{Codec: "pcm_s16le", SampleRate: 16000, Channels: 1, FrameMS: 20}}, nil
}

func (c *ASRClient) Close() { c.socket.close() }

// Recognize consumes negotiated PCM frames or complete Opus packets. Closing audio ends input early; recognition drains
// already accepted frames on its first sentence final. Only task-finished makes
// the returned transcript final. partial is never a Query trigger.
func (c *ASRClient) Recognize(ctx context.Context, audio <-chan []byte, partial func(string, bool), consumed func(int, int)) (text string, usages []chat.VoiceUsage, err error) {
	if !c.input.validInput() {
		return "", usages, errors.New("unsupported ASR input format")
	}
	if cap(audio) > c.input.startupFrames() {
		return "", usages, errors.New("voice startup audio overflow")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		finishProviderUsage(usages, err)
	}()
	// Audio stays buffered until task-started. Provider heartbeat keeps silent
	// PCM valid while the local recording bounds still limit each task.
	format := "pcm"
	if c.input.Codec == "opus" {
		format = "opus"
	}
	conn, end, usages, err := c.socket.start(ctx, map[string]any{"task_group": "audio", "task": "asr", "function": "recognition", "model": c.socket.model.ModelID, "input": map[string]any{}, "parameters": map[string]any{"format": format, "sample_rate": 16000, "semantic_punctuation_enabled": false, "max_sentence_silence": c.silenceMS, "heartbeat": true}})
	if err != nil {
		return "", usages, err
	}
	usage := &usages[len(usages)-1]
	id := usage.TaskID
	success := false
	defer func() { end(success) }()
	type result struct {
		event *dashScopeEvent
		err   error
	}
	events := make(chan result, 8)
	readDone := make(chan struct{})
	readCtx, stopReader := context.WithCancel(ctx)
	go func() {
		defer close(readDone)
		for {
			// Silence is valid while recording. The task context bounds recording;
			// finishTimer below bounds the response after finish-task.
			_, event, err := c.socket.read(conn, id, time.Time{}, usage)
			select {
			case events <- result{event, err}:
			case <-readCtx.Done():
				return
			}
			if err != nil || event != nil && event.Header.Event == "task-finished" {
				return
			}
		}
	}()
	defer func() {
		stopReader()
		if !success {
			_ = conn.Close()
		}
		<-readDone
	}()
	finishing := false
	totalBytes := 0
	totalSamples := 0
	input := audio
	finals := map[int]string{}
	finishedSentences := map[int]bool{}
	transcript := func() string {
		keys := make([]int, 0, len(finals))
		for key := range finals {
			keys = append(keys, key)
		}
		sort.Ints(keys)
		var out strings.Builder
		for _, key := range keys {
			out.WriteString(finals[key])
		}
		return out.String()
	}
	sendAudio := func(pcm []byte) error {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
			return errors.New("voice provider audio write failed")
		}
		return nil
	}
	var ogg *OpusOggWriter
	if format == "opus" {
		ogg, err = NewOpusOggWriter(16000, uint16(c.input.PreSkip48k), sendAudio)
		if err != nil {
			return "", usages, err
		}
	}
	accept := func(pcm []byte) error {
		samples, err := c.input.samples(pcm)
		if err != nil {
			return err
		}
		if consumed != nil {
			consumed(samples, len(pcm))
		}
		totalBytes += len(pcm)
		totalSamples += samples
		if totalBytes > maxRecordingAudioBytes || totalSamples > 60*48000 {
			return errRecordingLimit
		}
		if ogg != nil {
			return ogg.Write(pcm)
		}
		return sendAudio(pcm)
	}
	var finishTimer *time.Timer
	var finishedBy <-chan time.Time
	defer func() {
		if finishTimer != nil {
			finishTimer.Stop()
		}
	}()
	finish := func() error {
		if finishing {
			return nil
		}
		finishTimer = time.NewTimer(finishTimeout)
		finishedBy = finishTimer.C
		finishing = true
		input = nil
		if ogg != nil && totalSamples > 0 {
			if err := ogg.Finish(); err != nil {
				return err
			}
		}
		return dashCommand(conn, id, "finish-task", map[string]any{"input": map[string]any{}})
	}
	for {
		select {
		case <-ctx.Done():
			return "", usages, ctx.Err()
		case <-finishedBy:
			return "", usages, errors.New("voice recognition finish timeout")
		case pcm, ok := <-input:
			if !ok {
				input = nil
				if err = finish(); err != nil {
					return "", usages, err
				}
			} else if err = accept(pcm); err != nil {
				return "", usages, err
			}
		case result := <-events:
			if err := ctx.Err(); err != nil {
				return "", usages, err
			}
			if result.err != nil {
				return "", usages, result.err
			}
			event := result.event
			if event == nil {
				return "", usages, errors.New("unexpected ASR binary message")
			}
			switch event.Header.Event {
			case "task-started":
				return "", usages, errors.New("duplicate voice task-started")
			case "result-generated":
				sentence := event.Payload.Output.Sentence
				if sentence.Heartbeat || strings.TrimSpace(sentence.Text) == "" || finishedSentences[sentence.Begin] {
					continue
				}
				finals[sentence.Begin] = sentence.Text
				current := transcript()
				if utf8.RuneCountInString(current) > maxTranscriptChars {
					return "", usages, errors.New("voice transcript limit exceeded")
				}
				if partial != nil {
					partial(current, sentence.Final)
				}
				if sentence.Final {
					finishedSentences[sentence.Begin] = true
					if !finishing {
						// Only frames already buffered at this boundary belong to this utterance.
						for n := len(input); n > 0; n-- {
							pcm, ok := <-input
							if !ok {
								break
							}
							if err = accept(pcm); err != nil {
								return "", usages, err
							}
						}
						if err = finish(); err != nil {
							return "", usages, err
						}
					}
				}
			case "task-finished":
				if !finishing {
					return "", usages, errors.New("premature voice task-finished")
				}
				for key := range finals {
					if !finishedSentences[key] {
						delete(finals, key)
					}
				}
				success = true
				return transcript(), usages, nil
			}
		}
	}
}
