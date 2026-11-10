package chat

import (
	"errors"
	"os"
	"strings"
	"testing"

	"agent-platform/internal/models"
	"agent-platform/internal/modelusage"
)

func testVoiceUsage() VoiceUsage {
	return VoiceUsage{UsageID: "usage", Provider: "provider", TaskID: "task", Model: "tts", Operation: modelusage.OperationSpeech,
		Pricing: models.ModelPricing{Unit: models.PricingUnitPer10KCharacters, Currency: "CNY", Characters: 2}, Known: true, Characters: 10, Status: "running"}
}

func TestVoiceUsageCumulativeReplayNeverChangesModelBaseOrCreatesRun(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	initial, _, err := s.EnsureChat("chat", "agent", "hello")
	if err != nil {
		t.Fatal(err)
	}
	u := testVoiceUsage()
	for i, quantity := range []int{10, 10, 5, 20} {
		u.Characters = quantity
		changed, err := s.AppendVoiceUsage("chat", u)
		if err != nil || changed != (i == 0 || i == 3) {
			t.Fatalf("snapshot %d changed=%v err=%v", i, changed, err)
		}
	}
	u.Status = "complete"
	if _, err = s.AppendVoiceUsage("chat", u); err != nil {
		t.Fatal(err)
	}
	u.Status = "running"
	u.Characters = 1
	if changed, err := s.AppendVoiceUsage("chat", u); err != nil || changed {
		t.Fatal("late intermediate replaced terminal", err)
	}
	// The real Run binding may be learned after the provider's terminal result.
	u.RunID = "owned"
	if _, err = s.AppendVoiceUsage("chat", u); err != nil {
		t.Fatal(err)
	}
	detail, err := s.LoadChat("chat")
	if err != nil {
		t.Fatal(err)
	}
	if detail.ReplayUsage.LastRunID != "" || detail.ReplayUsage.Chat.TotalTokens != 0 || detail.ReplayUsage.Chat.EstimatedCostTotal != 0 {
		t.Fatal("voice modified model/Run base")
	}
	if detail.ReplayUsage.VoiceChat.Tasks != 1 || detail.ReplayUsage.VoiceChat.Characters != 20 || detail.ReplayUsage.VoiceByRun["owned"].Cost.Total != .004 {
		t.Fatalf("wrong projection: %+v", detail.ReplayUsage)
	}
	for _, event := range detail.Events {
		if strings.HasPrefix(event.Type, "run.") || event.Type == "voice.usage" {
			t.Fatal("voice fact entered transcript or created a Run")
		}
	}
	if len(detail.RawMessages) != 0 {
		t.Fatal("voice usage entered model messages")
	}
	summary, err := s.Summary("chat")
	if err != nil || summary.LastRunID != "" || summary.LastRunAt != 0 || summary.UpdatedAt != initial.UpdatedAt || summary.Usage != nil {
		t.Fatal("voice usage changed lifecycle/SQL base", err)
	}
	u.Pricing.Characters = 99
	if _, err = s.AppendVoiceUsage("chat", u); err == nil {
		t.Fatal("frozen price overwritten")
	}
}

func TestVoiceUsageUnknownAndDeletedChatDoNotBecomeZeroOrRevive(t *testing.T) {
	s, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u := testVoiceUsage()
	u.Known = false
	u.Characters = 0
	u.Status = "cancelled"
	if _, err = s.AppendVoiceUsage("missing", u); !errors.Is(err, ErrChatNotFound) {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.chatJSONLPath("missing")); !os.IsNotExist(err) {
		t.Fatal("reserved ID gained a file")
	}
	if _, _, err = s.EnsureChat("chat", "agent", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendVoiceUsage("chat", u); err != nil {
		t.Fatal(err)
	}
	detail, err := s.LoadChat("chat")
	if err != nil {
		t.Fatal(err)
	}
	if detail.ReplayUsage.VoiceChat.UnknownTasks != 1 || detail.ReplayUsage.VoiceChat.Cost != nil {
		t.Fatal("unknown usage claimed zero cost")
	}
	if err = s.DeleteChat("chat"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AppendVoiceUsage("chat", u); !errors.Is(err, ErrChatNotFound) {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.chatJSONLPath("chat")); !os.IsNotExist(err) {
		t.Fatal("late usage revived file")
	}
}

func TestVoiceUsageCostMergesOnceWithoutMutatingBase(t *testing.T) {
	base := &modelusage.EstimatedCost{Currency: "CNY", InputCacheHit: 1, Output: 2, Total: 3}
	voice := VoiceUsageTotals{Cost: &modelusage.EstimatedCost{Currency: "CNY", Total: .5}}
	for i := 0; i < 2; i++ {
		merged, err := MergeVoiceCost(base, voice)
		if err != nil || merged.Total != 3.5 || merged.InputCacheHit != 1 || merged.Output != 2 || base.Total != 3 {
			t.Fatal("base mutated or voice cost split into model prices", err)
		}
	}
	voice.Cost.Currency = "USD"
	if _, err := MergeVoiceCost(base, voice); err == nil {
		t.Fatal("mixed currencies summed")
	}
}
