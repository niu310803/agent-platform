package voice

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"agent-platform/internal/chat"
	"agent-platform/internal/models"
	"agent-platform/internal/modelusage"
	"github.com/gorilla/websocket"
)

type testUsageWriter struct {
	mu      sync.Mutex
	pending int
	values  []chat.VoiceUsage
}

func (w *testUsageWriter) BeginVoiceUsage(string) (func(error), error) {
	w.mu.Lock()
	w.pending++
	w.mu.Unlock()
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			if err == nil {
				w.mu.Lock()
				w.pending--
				w.mu.Unlock()
			}
		})
	}, nil
}
func (w *testUsageWriter) AppendVoiceUsage(_ string, u chat.VoiceUsage) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending <= 0 {
		return false, errors.New("missing usage hold")
	}
	w.values = append(w.values, u)
	return true, nil
}

func usageEvent(t *testing.T, c *websocket.Conn, id, kind string, usage any) {
	t.Helper()
	if err := c.WriteJSON(map[string]any{"header": map[string]any{"event": kind, "task_id": id}, "payload": map[string]any{"usage": usage}}); err != nil {
		t.Error(err)
	}
}

func TestProviderUsageCumulativeAndUnknown(t *testing.T) {
	for _, operation := range []string{modelusage.OperationSpeech, modelusage.OperationRecognition} {
		u := chat.VoiceUsage{Operation: operation, Status: "running"}
		for _, raw := range []string{`{"payload":{"usage":null}}`, `{"payload":{"usage":{}}}`} {
			var e dashScopeEvent
			if err := json.Unmarshal([]byte(raw), &e); err != nil {
				t.Fatal(err)
			}
			if err := recordProviderUsage(e, &u); err != nil || u.Known {
				t.Fatalf("unknown became known: %+v %v", u, err)
			}
		}
		for _, n := range []int{10, 10, 5, 20, 0} {
			var e dashScopeEvent
			data, _ := json.Marshal(map[string]any{"payload": map[string]any{"usage": map[string]any{"characters": n, "duration": n}}})
			if err := json.Unmarshal(data, &e); err != nil {
				t.Fatal(err)
			}
			if err := recordProviderUsage(e, &u); err != nil {
				t.Fatal(err)
			}
		}
		if !u.Known || operation == modelusage.OperationSpeech && (u.Characters != 20 || u.BilledAudioSeconds != 0) || operation == modelusage.OperationRecognition && (u.BilledAudioSeconds != 20 || u.Characters != 0) {
			t.Fatalf("wrong cumulative quantity: %+v", u)
		}
		values := []chat.VoiceUsage{u}
		finishProviderUsage(values, context.Canceled)
		if values[0].Status != "cancelled" || !values[0].Known {
			t.Fatal(values)
		}
	}
}

func TestTTSReturnsUsageOnFailureAndMissingTerminalUsage(t *testing.T) {
	for _, failed := range []bool{false, true} {
		p := fakeVoice(t, func(c *websocket.Conn) {
			cmd := readCommand(t, c, "run-task")
			event(t, c, cmd.Header.ID, "task-started", nil)
			readCommand(t, c, "continue-task")
			readCommand(t, c, "finish-task")
			for _, n := range []int{10, 10, 5, 20} {
				usageEvent(t, c, cmd.Header.ID, "result-generated", map[string]any{"characters": n})
			}
			// A stale task's quantity cannot contaminate the current task.
			usageEvent(t, c, "other-task", "result-generated", map[string]any{"characters": 999})
			if failed {
				usageEvent(t, c, cmd.Header.ID, "task-failed", nil)
				return
			}
			if err := c.WriteMessage(websocket.BinaryMessage, []byte{0, 0}); err != nil {
				t.Error(err)
			}
			usageEvent(t, c, cmd.Header.ID, "task-finished", nil)
		})
		usages, err := testTTS(t, p).Synthesize(context.Background(), "hello", "voice", 16000, func([]byte) error { return nil })
		if (err != nil) != failed || len(usages) != 1 || usages[0].Characters != 20 || !usages[0].Known {
			t.Fatalf("usages=%+v error=%v", usages, err)
		}
		want := "complete"
		if failed {
			want = "failed"
		}
		if usages[0].Status != want {
			t.Fatal(usages)
		}
	}
}

func TestASRCancellationReturnsAlreadyReadUsage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := fakeVoice(t, func(c *websocket.Conn) {
		cmd := readCommand(t, c, "run-task")
		event(t, c, cmd.Header.ID, "task-started", nil)
		usageEvent(t, c, cmd.Header.ID, "result-generated", map[string]any{"duration": 3.5})
		event(t, c, cmd.Header.ID, "result-generated", map[string]any{"text": "partial", "begin_time": 0})
		_, _, _ = c.ReadMessage()
	})
	_, usages, err := testASR(t, p).Recognize(ctx, make(chan []byte), func(string, bool) { cancel() }, nil)
	if !errors.Is(err, context.Canceled) || len(usages) != 1 || usages[0].Status != "cancelled" || !usages[0].Known || usages[0].BilledAudioSeconds != 3.5 {
		t.Fatalf("usages=%+v error=%v", usages, err)
	}
}

func TestProviderUsageFreezesPricePerTask(t *testing.T) {
	p := fakeVoice(t, func(c *websocket.Conn) {
		for i := 0; i < 2; i++ {
			cmd := readCommand(t, c, "run-task")
			event(t, c, cmd.Header.ID, "task-started", nil)
			readCommand(t, c, "continue-task")
			readCommand(t, c, "finish-task")
			_ = c.WriteMessage(websocket.BinaryMessage, []byte{0, 0})
			usageEvent(t, c, cmd.Header.ID, "task-finished", map[string]any{"characters": 2})
		}
	})
	client := testTTS(t, p)
	price := models.ModelPricing{Unit: models.PricingUnitPer10KCharacters, Characters: 1, Currency: "USD"}
	client.socket.pricing = func() (models.ModelPricing, error) { return price, nil }
	first, err := client.Synthesize(context.Background(), "hello", "voice", 16000, func([]byte) error { price.Characters = 2; return nil })
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.Synthesize(context.Background(), "hello", "voice", 16000, func([]byte) error { price.Characters = 3; return nil })
	if err != nil || first[0].Pricing.Characters != 1 || second[0].Pricing.Characters != 2 || second[0].Pricing.Currency != "USD" {
		t.Fatal(first, second, err)
	}
}
