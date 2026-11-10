package runexec

import (
	"strings"

	"agent-platform/internal/chat"
	"agent-platform/internal/config"
	"agent-platform/internal/contracts"
	"agent-platform/internal/models"
	"agent-platform/internal/modelusage"
	"agent-platform/internal/stream"
)

type UsageCostDecorator struct {
	Models  *models.ModelRegistry
	Billing config.BillingConfig
}

func (d UsageCostDecorator) DecorateCurrentUsage(data *stream.EventData) (chat.UsageData, bool) {
	if data == nil || data.Payload == nil {
		return chat.UsageData{}, false
	}
	usage, _ := data.Payload["usage"].(map[string]any)
	if usage == nil {
		return chat.UsageData{}, false
	}
	current, _ := usage["current"].(map[string]any)
	if current == nil {
		return chat.UsageData{}, false
	}
	currentUsage := UsageDataFromMap(current)
	if modelKey := UsageModelKeyFromEvent(data, current); modelKey != "" {
		currentUsage.ModelKey = modelKey
		current["modelKey"] = modelKey
	}
	currentUsage = d.EstimateForModel(currentUsage)
	if estimated := UsageEstimatedCostFromData(currentUsage); estimated != nil {
		current["estimatedCost"] = estimated
	}
	return currentUsage, true
}

func (d UsageCostDecorator) DecorateDebugLLMReturnUsage(inner map[string]any) (chat.UsageData, bool) {
	if inner == nil {
		return chat.UsageData{}, false
	}
	usage, _ := inner["usage"].(map[string]any)
	if usage == nil {
		return chat.UsageData{}, false
	}
	llmReturnUsage, _ := usage["llmReturnUsage"].(map[string]any)
	if llmReturnUsage == nil {
		return chat.UsageData{}, false
	}
	currentUsage := UsageDataFromMap(llmReturnUsage)
	if modelKey := UsageModelKeyFromDebugData(inner, llmReturnUsage); modelKey != "" {
		currentUsage.ModelKey = modelKey
		llmReturnUsage["modelKey"] = modelKey
	}
	currentUsage = d.EstimateForModel(currentUsage)
	if estimated := UsageEstimatedCostFromData(currentUsage); estimated != nil {
		llmReturnUsage["estimatedCost"] = estimated
	}
	return currentUsage, true
}

func (d UsageCostDecorator) EstimateForModel(usage chat.UsageData) chat.UsageData {
	if d.Models == nil {
		return usage
	}
	modelKey := strings.TrimSpace(usage.ModelKey)
	if modelKey == "" || !UsageHasBillableUsage(usage) {
		return usage
	}
	model, err := d.Models.GetModel(modelKey)
	if err != nil || !modelusage.PricingEnabled(model.Pricing) {
		return usage
	}
	hit := usage.PromptCacheHitTokens
	if hit <= 0 {
		hit = usage.CachedTokens
	}
	cost := modelusage.EstimateCost(contracts.ModelUsage{PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		CacheHitTokens: hit, CacheMissTokens: usage.PromptCacheMissTokens, Characters: usage.Characters, BilledAudioSeconds: usage.BilledAudioSeconds}, model.Pricing, d.Billing.Currency)
	if cost != nil {
		usage.EstimatedCostCurrency, usage.EstimatedCostTotal = cost.Currency, cost.Total
		usage.EstimatedCostInputHit, usage.EstimatedCostInputMiss, usage.EstimatedCostOutput = cost.InputCacheHit, cost.InputCacheMiss, cost.Output
	}
	return usage
}

func UsageModelKeyFromEvent(data *stream.EventData, usage map[string]any) string {
	if data == nil || data.Payload == nil {
		return ""
	}
	usageRoot, _ := data.Payload["usage"].(map[string]any)
	modelNode, _ := data.Payload["model"].(map[string]any)
	return strings.TrimSpace(contracts.FirstNonEmptyString(usage["modelKey"], usageRoot["modelKey"], modelNode["key"]))
}

func UsageModelKeyFromDebugData(inner map[string]any, usage map[string]any) string {
	if inner == nil {
		return ""
	}
	modelNode, _ := inner["model"].(map[string]any)
	return strings.TrimSpace(contracts.FirstNonEmptyString(usage["modelKey"], modelNode["key"]))
}

func UsageHasBillableUsage(usage chat.UsageData) bool {
	return usage.PromptTokens > 0 || usage.CompletionTokens > 0 || usage.TotalTokens > 0 ||
		usage.CachedTokens > 0 || usage.PromptCacheHitTokens > 0 || usage.PromptCacheMissTokens > 0 ||
		usage.Characters > 0 || usage.BilledAudioSeconds > 0
}
