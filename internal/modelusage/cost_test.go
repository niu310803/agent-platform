package modelusage

import (
	"math"
	"testing"

	"agent-platform/internal/contracts"
	"agent-platform/internal/models"
)

func TestPriceProviderAudioSecondsWithoutAddingTokens(t *testing.T) {
	pricing := models.ParseModelPricing(map[string]any{"unit": "per_second", "seconds": .125, "currency": "usd"})
	if err := models.ValidateModelPricing(pricing, models.ModelTypeASR); err != nil {
		t.Fatal(err)
	}
	usage := contracts.ModelUsage{BilledAudioSeconds: 2.5}
	if !usage.HasBillableUsage() || usage.HasTokens() {
		t.Fatal("audio duration treated as tokens")
	}
	cost := EstimateCost(usage, pricing, "CNY")
	if cost == nil || cost.Total != .3125 || cost.Currency != "USD" || cost.InputCacheHit != 0 || cost.InputCacheMiss != 0 || cost.Output != 0 {
		t.Fatalf("invalid audio cost: %+v", cost)
	}
	if EstimateCost(usage, models.ModelPricing{}, "CNY") != nil {
		t.Fatal("missing pricing claimed zero cost")
	}
}

func TestNeutralPricingRetainsTokenAndCharacterCalculations(t *testing.T) {
	usage := contracts.ModelUsage{PromptTokens: 1000, CompletionTokens: 200, CacheHitTokens: 400, Characters: 50}
	cost := EstimateCost(usage, models.ModelPricing{InputCacheHit: 1, InputCacheMiss: 2, Output: 3}, "usd")
	if cost.Currency != "USD" || math.Abs(cost.Total-.0022) > 1e-12 {
		t.Fatalf("token cost: %+v", cost)
	}
	cost = EstimateCost(usage, models.ModelPricing{Unit: models.PricingUnitPer10KCharacters, Characters: 2}, "")
	if cost.Currency != "CNY" || cost.Total != .01 || cost.Output != 0 {
		t.Fatalf("character cost: %+v", cost)
	}
}
