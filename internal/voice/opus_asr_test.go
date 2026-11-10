package voice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOpusASRUsesOggAndPreservesPacketDurationAndUsage(t *testing.T) {
	packets := [][]byte{{0xf8, 1}, {0x58, 2}, {0xfb, 2, 1, 2}}
	p := fakeVoice(t, func(c *websocket.Conn) {
		start := readCommand(t, c, "run-task")
		id := start.Header.ID
		if start.Payload.Parameters["format"] != "opus" || start.Payload.Parameters["sample_rate"] != float64(16000) || start.Payload.Parameters["heartbeat"] != true {
			t.Error(start.Payload.Parameters)
		}
		event(t, c, id, "task-started", nil)
		var data []byte
		for {
			kind, chunk, err := c.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if kind == websocket.BinaryMessage {
				data = append(data, chunk...)
				continue
			}
			var cmd map[string]any
			if err = json.Unmarshal(chunk, &cmd); err != nil {
				t.Error(err)
				return
			}
			if cmd["header"].(map[string]any)["action"] != "finish-task" {
				t.Error("unexpected command")
			}
			break
		}
		info, err := InspectOpusOgg(data)
		if err != nil {
			t.Error(err)
		}
		if info.PreSkip != 312 || info.DecodedSamples != 5760 || info.Packets != 3 || !info.EOS {
			t.Errorf("unexpected Ogg: %+v", info)
		}
		var got [][]byte
		r := &oggOpusReader{onPacket: func(p []byte, _ int) error { got = append(got, bytes.Clone(p)); return nil }}
		if err = r.write(data); err != nil {
			t.Error(err)
		}
		if !bytes.Equal(bytes.Join(got, nil), bytes.Join(packets, nil)) {
			t.Error("encoded packets were changed")
		}
		usageEvent(t, c, id, "result-generated", map[string]any{"duration": 2})
		event(t, c, id, "result-generated", map[string]any{"text": "encoded answer", "sentence_end": true})
		event(t, c, id, "task-finished", nil)
	})
	c := testASR(t, p)
	c.input = audioFormat{Codec: "opus", SampleRate: 16000, Channels: 1, PreSkip48k: 312}
	input := make(chan []byte, len(packets))
	for _, packet := range packets {
		input <- packet
	}
	close(input)
	consumedSamples, consumedBytes := 0, 0
	text, usages, err := c.Recognize(context.Background(), input, nil, func(samples, n int) { consumedSamples += samples; consumedBytes += n })
	if err != nil || text != "encoded answer" || consumedSamples != 5760 || consumedBytes != 8 {
		t.Fatalf("text=%q consumed=%d/%d err=%v", text, consumedSamples, consumedBytes, err)
	}
	if len(usages) != 1 || !usages[0].Known || usages[0].BilledAudioSeconds != 2 {
		t.Fatalf("usage=%+v", usages)
	}
}

func TestOpusASRDurationLimitDoesNotUseCompressedByteCount(t *testing.T) {
	p := fakeVoice(t, func(c *websocket.Conn) {
		cmd := readCommand(t, c, "run-task")
		event(t, c, cmd.Header.ID, "task-started", nil)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	})
	c := testASR(t, p)
	c.input = audioFormat{Codec: "opus", SampleRate: 16000, Channels: 1}
	input := make(chan []byte, 1001)
	for i := 0; i < 1001; i++ {
		input <- []byte{0x58}
	}
	close(input) // 60ms DTX packets: only 1001 bytes, >60 seconds.
	_, _, err := c.Recognize(context.Background(), input, nil, nil)
	if !errors.Is(err, errRecordingLimit) {
		t.Fatalf("duration bound lost: %v", err)
	}
}

func TestOpusASRAutomaticFinalFlushesLastPacket(t *testing.T) {
	p := fakeVoice(t, func(c *websocket.Conn) {
		cmd := readCommand(t, c, "run-task")
		id := cmd.Header.ID
		event(t, c, id, "task-started", nil)
		var data []byte
		pages := 0
		for {
			kind, chunk, err := c.ReadMessage()
			if err != nil {
				t.Error(err)
				return
			}
			if kind == websocket.BinaryMessage {
				data = append(data, chunk...)
				pages++
				if pages == 3 {
					event(t, c, id, "result-generated", map[string]any{"text": "自动结束", "sentence_end": true})
				}
				continue
			}
			var finish testCommand
			if json.Unmarshal(chunk, &finish) != nil || finish.Header.Action != "finish-task" {
				t.Error("unexpected finish")
			}
			break
		}
		info, err := InspectOpusOgg(data)
		if err != nil || info.Packets != 2 || info.DecodedSamples != 1920 || !info.EOS {
			t.Errorf("lost held packet: %+v %v", info, err)
		}
		event(t, c, id, "task-finished", nil)
	})
	c := testASR(t, p)
	c.input = audioFormat{Codec: "opus", SampleRate: 16000, Channels: 1}
	input := make(chan []byte, 2)
	input <- []byte{0xf8, 1}
	input <- []byte{0xf8, 2}
	text, _, err := c.Recognize(context.Background(), input, nil, nil)
	if err != nil || text != "自动结束" {
		t.Fatalf("%q %v", text, err)
	}
}

func TestVoiceOpusQueueBoundsDurationAndBytesIndependently(t *testing.T) {
	for _, kind := range []string{"duration", "bytes"} {
		t.Run(kind, func(t *testing.T) {
			s, client := eventSession(t, Dependencies{})
			s.asr.input = audioFormat{Codec: "opus", SampleRate: 16000, Channels: 1}
			s.current = mediaTurn(t, 1)
			s.current.audio = make(chan []byte, s.asr.input.startupFrames())
			packet := []byte{0x58}
			limit := int(maxStartupAudio / (60 * time.Millisecond))
			if kind == "bytes" {
				packet = append([]byte{0x80}, make([]byte, 1275)...)
				limit = maxStartupAudioBytes / len(packet)
			}
			for i := 0; i < limit; i++ {
				if err := s.audio(packet); err != nil {
					t.Fatal(err)
				}
			}
			if int64(s.current.inputSamples) != s.current.queuedSamples.Load() || int64(s.current.inputBytes) != s.current.queuedBytes.Load() {
				t.Fatal("queue accounting mismatch")
			}
			_ = s.audio(packet)
			frame := eventFrame(t, client)
			if frame["code"] != "audio_overflow" {
				t.Fatal(frame)
			}
		})
	}
}
