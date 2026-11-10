package chat

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"

	"agent-platform/internal/contracts"
	"agent-platform/internal/contracts/queryinput"
	"agent-platform/internal/models"
	"agent-platform/internal/modelusage"
)

// VoiceUsage is a cumulative provider task fact, independent of Run completion.
// Known distinguishes a reported zero from unavailable billing quantities.
type VoiceUsage struct {
	UsageID            string              `json:"usageId"`
	Provider           string              `json:"provider"`
	TaskID             string              `json:"taskId"`
	Model              string              `json:"model"`
	Operation          string              `json:"operation"`
	RunID              string              `json:"runId,omitempty"`
	Pricing            models.ModelPricing `json:"pricing"`
	Known              bool                `json:"known"`
	Characters         int                 `json:"characters"`
	BilledAudioSeconds float64             `json:"billedAudioSeconds"`
	Status             string              `json:"status"`
}

type VoiceUsageTotals struct {
	Tasks              int                       `json:"tasks"`
	UnknownTasks       int                       `json:"unknownTasks"`
	PendingTasks       int                       `json:"pendingTasks"`
	UnpricedTasks      int                       `json:"unpricedTasks"`
	Characters         int                       `json:"characters"`
	BilledAudioSeconds float64                   `json:"billedAudioSeconds"`
	Cost               *modelusage.EstimatedCost `json:"estimatedCost,omitempty"`
}

func (u VoiceUsage) validate() error {
	for _, value := range []string{u.UsageID, u.Provider, u.TaskID, u.Model, u.Operation} {
		if value == "" || strings.TrimSpace(value) != value || len(value) > 256 {
			return errors.New("invalid voice usage identity")
		}
	}
	if len(u.RunID) > 256 || strings.TrimSpace(u.RunID) != u.RunID {
		return errors.New("invalid voice usage Run")
	}
	if u.Status != "running" && u.Status != "complete" && u.Status != "failed" && u.Status != "cancelled" {
		return errors.New("invalid voice usage status")
	}
	if u.Characters < 0 || u.BilledAudioSeconds < 0 || math.IsNaN(u.BilledAudioSeconds) || math.IsInf(u.BilledAudioSeconds, 0) {
		return errors.New("invalid voice usage quantity")
	}
	if !u.Known && (u.Characters != 0 || u.BilledAudioSeconds != 0) {
		return errors.New("unknown voice usage carries quantities")
	}
	modelType := models.ModelTypeASR
	switch u.Operation {
	case modelusage.OperationRecognition:
		if u.Characters != 0 {
			return errors.New("ASR usage must report duration")
		}
	case modelusage.OperationSpeech:
		modelType = models.ModelTypeTTS
		if u.BilledAudioSeconds != 0 {
			return errors.New("TTS usage must report characters")
		}
	default:
		return errors.New("invalid voice usage operation")
	}
	for _, price := range []float64{u.Pricing.InputCacheHit, u.Pricing.InputCacheMiss, u.Pricing.Output, u.Pricing.Characters, u.Pricing.Seconds} {
		if price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return errors.New("invalid voice usage price")
		}
	}
	if err := models.ValidateModelPricing(u.Pricing, modelType); err != nil {
		return err
	}
	if cost := modelusage.EstimateCost(contracts.ModelUsage{Characters: u.Characters, BilledAudioSeconds: u.BilledAudioSeconds}, u.Pricing, ""); cost != nil && (math.IsInf(cost.Total, 0) || math.IsNaN(cost.Total)) {
		return errors.New("voice usage cost overflow")
	}
	return nil
}

// foldVoiceUsage is shared by writes and replay: quantities never decrease,
// binding may advance from Chat-only to a Run, and terminal status never regresses.
func foldVoiceUsage(previous *VoiceUsage, next VoiceUsage) (VoiceUsage, error) {
	if err := next.validate(); err != nil {
		return VoiceUsage{}, err
	}
	if previous == nil {
		return next, nil
	}
	old := *previous
	if old.UsageID != next.UsageID || old.TaskID != next.TaskID || old.Provider != next.Provider || old.Model != next.Model || old.Operation != next.Operation || old.Pricing != next.Pricing {
		return VoiceUsage{}, errors.New("voice usage identity or frozen price changed")
	}
	if old.RunID != "" {
		if next.RunID != "" && next.RunID != old.RunID {
			return VoiceUsage{}, errors.New("voice usage Run changed")
		}
		next.RunID = old.RunID
	}
	if old.Status != "running" {
		if next.Status != "running" && next.Status != old.Status {
			return VoiceUsage{}, errors.New("conflicting voice usage terminal status")
		}
		next.Status = old.Status
	}
	next.Known = old.Known || next.Known
	next.Characters = max(old.Characters, next.Characters)
	next.BilledAudioSeconds = max(old.BilledAudioSeconds, next.BilledAudioSeconds)
	return next, nil
}

func voiceUsageFromEvent(event map[string]any) (VoiceUsage, error) {
	data, err := json.Marshal(event["usage"])
	if err != nil {
		return VoiceUsage{}, err
	}
	var usage VoiceUsage
	if err := queryinput.DecodeStrictJSON(data, &usage); err != nil {
		return VoiceUsage{}, errors.New("invalid voice usage payload")
	}
	return usage, usage.validate()
}

func foldVoiceEvent(values map[string]VoiceUsage, event map[string]any) error {
	u, err := voiceUsageFromEvent(event)
	if err != nil {
		return err
	}
	return foldVoiceValue(values, u)
}

func foldVoiceValue(values map[string]VoiceUsage, u VoiceUsage) error {
	for id, existing := range values {
		if id != u.UsageID && existing.Provider == u.Provider && existing.TaskID == u.TaskID {
			return errors.New("provider task has multiple voice usage identities")
		}
	}
	var previous *VoiceUsage
	if old, ok := values[u.UsageID]; ok {
		previous = &old
	}
	u, err := foldVoiceUsage(previous, u)
	if err == nil {
		values[u.UsageID] = u
	}
	return err
}

// AppendVoiceUsage uses the same Store lock as deletion. Only an existing SQL
// Chat may gain a JSONL row; a reserved/deleted ID can never recreate a Chat.
func (s *FileStore) AppendVoiceUsage(chatID string, usage VoiceUsage) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !ValidChatID(chatID) {
		return false, os.ErrPermission
	}
	summary, err := s.loadSummary(chatID)
	if err != nil {
		return false, err
	}
	if summary == nil {
		return false, ErrChatNotFound
	}
	lines, err := readPersistedJSONLines(s.chatJSONLPath(chatID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	values := map[string]VoiceUsage{}
	for _, line := range lines {
		event, _ := line["event"].(map[string]any)
		if line["_type"] == "event" && event["type"] == "voice.usage" {
			if err := foldVoiceEvent(values, event); err != nil {
				return false, err
			}
		}
	}
	var previous *VoiceUsage
	if old, ok := values[usage.UsageID]; ok {
		previous = &old
	}
	if err := foldVoiceValue(values, usage); err != nil {
		return false, err
	}
	merged := values[usage.UsageID]
	if previous != nil && *previous == merged {
		return false, nil
	}
	now := time.Now().UnixMilli()
	line := EventLine{Type: "event", ChatID: chatID, RunID: merged.RunID, UpdatedAt: now, Event: map[string]any{"type": "voice.usage", "timestamp": now, "chatId": chatID, "usage": merged}}
	// The guarded existing Chat may have no prior events. Reuse the existing
	// writer while retaining the Store lock and the SQL existence check above.
	if err := s.appendJSONLineLocked(s.chatJSONLPath(chatID), line); err != nil {
		return false, err
	}
	return true, nil
}

func voiceUsageTotals(values map[string]VoiceUsage) (VoiceUsageTotals, map[string]VoiceUsageTotals, error) {
	var total VoiceUsageTotals
	var byRun map[string]VoiceUsageTotals
	ids := make([]string, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		u := values[id]
		one := VoiceUsageTotals{Tasks: 1, Characters: u.Characters, BilledAudioSeconds: u.BilledAudioSeconds}
		if u.Status == "running" {
			one.PendingTasks = 1
		}
		if !u.Known {
			one.UnknownTasks = 1
		} else {
			one.Cost = modelusage.EstimateCost(contracts.ModelUsage{Characters: u.Characters, BilledAudioSeconds: u.BilledAudioSeconds}, u.Pricing, "")
			if one.Cost == nil {
				one.UnpricedTasks = 1
			}
		}
		var err error
		if total, err = addVoiceTotals(total, one); err != nil {
			return total, nil, err
		}
		if u.RunID != "" {
			if byRun == nil {
				byRun = map[string]VoiceUsageTotals{}
			}
			if byRun[u.RunID], err = addVoiceTotals(byRun[u.RunID], one); err != nil {
				return total, nil, err
			}
		}
	}
	return total, byRun, nil
}

func addVoiceTotals(a, b VoiceUsageTotals) (VoiceUsageTotals, error) {
	var err error
	a.Cost, err = MergeVoiceCost(a.Cost, b)
	a.Tasks += b.Tasks
	a.UnknownTasks += b.UnknownTasks
	a.PendingTasks += b.PendingTasks
	a.UnpricedTasks += b.UnpricedTasks
	if b.Characters > int(^uint(0)>>1)-a.Characters {
		return a, errors.New("voice usage quantity overflow")
	}
	a.Characters += b.Characters
	a.BilledAudioSeconds += b.BilledAudioSeconds
	if math.IsInf(a.BilledAudioSeconds, 0) {
		return a, errors.New("voice usage duration overflow")
	}
	return a, err
}

// MergeVoiceCost is called once, after choosing the ordinary model/tool base.
// Never persist the merged result into the ordinary SQL usage columns.
func MergeVoiceCost(base *modelusage.EstimatedCost, voice VoiceUsageTotals) (*modelusage.EstimatedCost, error) {
	if base == nil && voice.Cost == nil {
		return nil, nil
	}
	var cost modelusage.EstimatedCost
	if base != nil {
		cost = *base
	}
	if voice.Cost != nil {
		if cost.Currency != "" && cost.Currency != voice.Cost.Currency {
			return nil, fmt.Errorf("voice usage currency differs from base")
		}
		cost.Currency = voice.Cost.Currency
		cost.Total += voice.Cost.Total
		if math.IsInf(cost.Total, 0) || math.IsNaN(cost.Total) {
			return nil, errors.New("voice usage cost overflow")
		}
	}
	return &cost, nil
}
