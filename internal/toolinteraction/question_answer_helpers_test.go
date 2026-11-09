package toolinteraction

import (
	"encoding/json"
	"math"
	"testing"
)

func TestQuestionSharedValidationRejectsInvalidCalendarsAndNumbers(t *testing.T) {
	for _, tc := range []struct {
		kind  string
		value any
	}{
		{"date", "2026-02-29"}, {"date", "2026-13-01"}, {"date", "2026-2-01"}, {"date", "0000-01-01"},
		{"datetime", "2026-11-07 25:00:00"}, {"datetime", "2026-11-31T09:30"}, {"datetime", "明天下午"},
		{"number", math.NaN()}, {"number", math.Inf(1)}, {"number", json.Number("1e999")}, {"number", "12"},
	} {
		if _, err := NormalizeQuestionSubmitItem(map[string]any{"type": tc.kind}, map[string]any{"answer": tc.value}); err == nil {
			t.Fatalf("accepted invalid %s: %v", tc.kind, tc.value)
		}
	}
	for _, tc := range []struct {
		kind  string
		value any
	}{
		{"date", "2028-02-29"}, {"datetime", "2026-11-07 15:00:00"}, {"datetime", "2026-11-07T15:00"}, {"number", -12.5}, {"number", json.Number("12")},
	} {
		if _, err := NormalizeQuestionSubmitItem(map[string]any{"type": tc.kind}, map[string]any{"answer": tc.value}); err != nil {
			t.Fatal(tc, err)
		}
	}
}
