package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestVoiceOpusNegotiationAndPersistedPCMAnswer(t *testing.T) {
	f := newTestFixtureWithModelHandler(t, func(w http.ResponseWriter, r *http.Request) {
		writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"最终正文。"},"finish_reason":"stop"}]}`, "[DONE]")
	})
	srv, p := enableVoiceFixture(t, f, nil)
	hello := voiceHello("", 16000)
	input := map[string]any{"codec": "opus", "sampleRate": 16000, "channels": 1, "preSkip48k": 312}
	hello["audio"].(map[string]any)["input"] = input
	reject := func() {
		t.Helper()
		c, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/api/voice/ws", nil)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if err = c.WriteJSON(hello); err != nil {
			t.Fatal(err)
		}
		msg, _ := readVoice(t, c)
		if msg["code"] != "invalid_request" {
			t.Fatal(msg)
		}
	}
	if slices.Contains(f.server.deps.Voice.Capabilities()["input"].(map[string]any)["codecs"].([]string), "opus") {
		t.Fatal("unverified model advertised Opus")
	}
	reject()
	path := filepath.Join(f.cfg.Paths.RegistriesDir, "models/voice-asr.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "configured-asr", "paraformer-realtime-v2")), 0600); err != nil {
		t.Fatal(err)
	}
	if err = f.modelRegistry.ReloadModels(); err != nil {
		t.Fatal(err)
	}
	cap := f.server.deps.Voice.Capabilities()
	if !slices.Contains(cap["input"].(map[string]any)["codecs"].([]string), "opus") || slices.Contains(cap["output"].(map[string]any)["codecs"].([]string), "opus") {
		t.Fatal(cap)
	}
	for _, field := range []string{"frameMs", "preSkip48k", "sampleRate", "channels"} {
		original, had := input[field]
		input[field] = map[string]int{"frameMs": 20, "preSkip48k": 65536, "sampleRate": 24000, "channels": 2}[field]
		reject()
		if had {
			input[field] = original
		} else {
			delete(input, field)
		}
	}
	output := hello["audio"].(map[string]any)["output"].(map[string]any)
	output["codec"] = "opus"
	reject()
	output["codec"] = "pcm_s16le"
	if p.asr.Load() != 0 || p.tts.Load() != 0 {
		t.Fatal("invalid hello contacted provider")
	}
	c, chatID := dialVoiceHello(t, srv, hello)
	if err = startVoice(t, c); err != nil {
		t.Fatal(err)
	}
	for _, packet := range [][]byte{{0xf8, 1}, {0x58, 2}, {0xfb, 2, 1, 2}} {
		if err = c.WriteMessage(websocket.BinaryMessage, packet); err != nil {
			t.Fatal(err)
		}
	}
	if err = stopVoice(c); err != nil {
		t.Fatal(err)
	}
	start, _ := voiceUntil(t, c, "tts", "start")
	if start["text"] != "最终正文。" {
		t.Fatal(start)
	}
	stop, frames := voiceUntil(t, c, "tts", "stop")
	got, _ := json.Marshal(frames)
	if stop["reason"] != "done" || string(got) != "[640,640,2]" || len(queryLines(t, f, chatID)) != 1 || p.asr.Load() != 1 || p.tts.Load() != 1 {
		t.Fatalf("stop=%v frames=%s", stop, got)
	}
}
