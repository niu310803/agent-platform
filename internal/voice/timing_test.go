package voice

import (
	"bytes"
	"encoding/json"
	"log"
	"strings"
	"testing"
	"time"

	"agent-platform/internal/chat"
	rt "agent-platform/internal/runtime/types"
	"github.com/gorilla/websocket"
)

func TestVoiceTimingFirstAudioAndReleaseUseActualPCM(t *testing.T) {
	var output bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(writer); log.SetFlags(flags) })
	provider := fakeVoice(t, func(c *websocket.Conn) {
		id := readCommand(t, c, "run-task").Header.ID
		event(t, c, id, "task-started", nil)
		readCommand(t, c, "continue-task")
		readCommand(t, c, "finish-task")
		_ = c.WriteMessage(websocket.BinaryMessage, make([]byte, 1280))
		event(t, c, id, "task-finished", nil)
	})
	s, client := eventSession(t, Dependencies{Chats: resultChats{}, Runs: recoveryRuns{}})
	s.started, s.id = time.Now(), "timing-session"
	s.tts, s.voice, s.sampleRate = testTTS(t, provider), "voice", 16000
	s.current = mediaTurn(t, 2)
	if err := s.finishRun(s.current, rt.QueryResult{RunID: "completed-run", Persisted: true, FinishReason: "complete", Completion: &chat.RunCompletion{AssistantText: "private final text"}}, nil); err != nil {
		t.Fatal(err)
	}
	start := eventFrame(t, client)
	pcm := nextInputEvent(t, s) // Producer blocks for the original single-writer acknowledgement.
	if pcm.kind != "pcm" {
		t.Fatal(pcm.kind)
	}
	beforeRelease := float64(time.Since(s.started).Microseconds()) / 1000
	for {
		if err := s.event(pcm); err != nil {
			t.Fatal(err)
		}
		if pcm.kind == "pcm" {
			if kind, _, err := client.ReadMessage(); err != nil || kind != websocket.BinaryMessage {
				t.Fatal(kind, err)
			}
		} else if pcm.kind == "spoken" {
			eventFrame(t, client)
			break
		}
		pcm = nextInputEvent(t, s)
	}
	s.service.Close()
	counts := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err, line)
		}
		if entry["category"] != "voice.timing" {
			continue
		}
		stage := entry["stage"].(string)
		counts[stage]++
		if entry["speechId"] != start["speechId"] || entry["runId"] != "completed-run" || entry["turn"] != float64(2) || entry["providerTaskId"] == "" || entry["providerTaskId"] == nil || entry["clock"] != "server_session_monotonic" {
			t.Fatal(entry)
		}
		at := entry["atMs"].(float64)
		if at < 0 || stage == "ttsFirstAudio" && at > beforeRelease || stage == "audioReleased" && at < beforeRelease {
			t.Fatal("incorrect stage boundary", entry, beforeRelease)
		}
	}
	if counts["ttsFirstAudio"] != 1 || counts["audioReleased"] != 1 || counts["ttsFinished"] != 1 || len(counts) != 3 {
		t.Fatal(counts)
	}
	if strings.Contains(output.String(), "private final text") || strings.Contains(output.String(), "test-secret") {
		t.Fatal("timing leaked content or credentials")
	}
}

func TestVoiceTimingKeepsCancelledSpeechWithoutClaimingAudioRelease(t *testing.T) {
	var output bytes.Buffer
	writer, flags := log.Writer(), log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(writer); log.SetFlags(flags) })
	provider := fakeVoice(t, func(c *websocket.Conn) {
		id := readCommand(t, c, "run-task").Header.ID
		event(t, c, id, "task-started", nil)
		readCommand(t, c, "continue-task")
		readCommand(t, c, "finish-task")
		_ = c.WriteMessage(websocket.BinaryMessage, make([]byte, 640))
		event(t, c, id, "task-finished", nil)
	})
	s, client := eventSession(t, Dependencies{Chats: resultChats{}, Runs: recoveryRuns{}})
	s.started, s.id = time.Now(), "cancelled-session"
	s.tts, s.voice, s.sampleRate = testTTS(t, provider), "voice", 16000
	s.current = mediaTurn(t, 1)
	if err := s.speak(s.current, "private body", "final", "run", ""); err != nil {
		t.Fatal(err)
	}
	eventFrame(t, client)
	pcm := nextInputEvent(t, s)
	if pcm.kind != "pcm" {
		t.Fatal(pcm.kind)
	}
	s.invalidate() // Superseded before the single writer releases the first frame.
	s.service.Close()
	if err := s.event(pcm); err != nil {
		t.Fatal(err)
	}
	var finished bool
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["stage"] == "audioReleased" {
			t.Fatal("unplayed PCM counted as released")
		}
		if entry["stage"] == "ttsFinished" {
			finished = true
			if entry["failed"] != true || entry["cancelled"] != true {
				t.Fatal(entry)
			}
		}
	}
	if !finished {
		t.Fatal("cancelled sample lost from diagnostics")
	}
}
