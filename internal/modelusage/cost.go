package modelusage

import (
	"strings"

	"agent-platform/internal/contracts"
	"agent-platform/internal/models"
)

// EstimatedCost is the common pure price calculation for model, tool and voice
// usage. Persistence and projection remain with their existing owners.
type EstimatedCost struct {
	Currency       string  `json:"currency"`
	InputCacheHit  float64 `json:"inputCacheHit"`
	InputCacheMiss float64 `json:"inputCacheMiss"`
	Output         float64 `json:"output"`
	Total          float64 `json:"total"`
}

func PricingEnabled(pricing models.ModelPricing) bool {
	switch pricing.Unit {
	case models.PricingUnitPer10KCharacters:
		return pricing.Characters > 0
	case models.PricingUnitPerSecond:
		return pricing.Seconds > 0
	default:
		return pricing.InputCacheHit > 0 || pricing.InputCacheMiss > 0 || pricing.Output > 0
	}
}

// EstimateCost consumes provider quantities and a frozen price, never local
// text/audio length. A nil cost means pricing is unavailable, not a free call.
func EstimateCost(usage contracts.ModelUsage, pricing models.ModelPricing, fallbackCurrency string) *EstimatedCost {
	if !PricingEnabled(pricing) {
		return nil
	}
	currency := PricingCurrency(pricing, fallbackCurrency)
	cost := &EstimatedCost{Currency: currency}
	switch pricing.Unit {
	case models.PricingUnitPer10KCharacters:
		cost.Total = float64(usage.Characters) * pricing.Characters / 10_000
	case models.PricingUnitPerSecond:
		cost.Total = usage.BilledAudioSeconds * pricing.Seconds
	default:
		hit, miss := usage.CacheHitTokens, usage.CacheMissTokens
		if hit <= 0 && miss <= 0 {
			miss = usage.PromptTokens
		} else if miss <= 0 && usage.PromptTokens > hit {
			miss = usage.PromptTokens - hit
		}
		cost.InputCacheHit = float64(hit) * pricing.InputCacheHit / 1_000_000
		cost.InputCacheMiss = float64(miss) * pricing.InputCacheMiss / 1_000_000
		cost.Output = float64(usage.CompletionTokens) * pricing.Output / 1_000_000
		cost.Total = cost.InputCacheHit + cost.InputCacheMiss + cost.Output
	}
	return cost
}

// PricingCurrency resolves the existing billing fallback before task snapshots are frozen.
func PricingCurrency(pricing models.ModelPricing, fallbackCurrency string) string {
	currency := strings.ToUpper(strings.TrimSpace(pricing.Currency))
	if currency == "" {
		currency = strings.ToUpper(strings.TrimSpace(fallbackCurrency))
	}
	if currency == "" {
		currency = "CNY"
	}
	return currency
}
