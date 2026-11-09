package voice

import (
	"reflect"
	"testing"
	"time"
)

func TestQuestionSpokenAnswers(t *testing.T) {
	finalized := time.Date(2026, 11, 6, 17, 30, 0, 0, time.UTC) // Nov 7 in Shanghai.
	options := []any{map[string]any{"label": "苹果"}, map[string]any{"label": "香蕉"}, map[string]any{"label": "和平"}}
	for _, tt := range []struct {
		kind, speech, zone string
		free               bool
		want               any
	}{
		{"text", "这里有两句话。保留内容。", "", false, "这里有两句话。保留内容"},
		{"select", "第二个", "", false, "香蕉"},
		{"select", "和平", "", false, "和平"},
		{"select", "其他：梨", "", true, "梨"},
		{"multi-select", "第一和第三", "", false, []any{"苹果", "和平"}},
		{"multi-select", "苹果、香蕉、苹果", "", false, []any{"苹果", "香蕉"}},
		{"multi-select", "苹果和其他：梨", "", true, []any{"苹果", "梨"}},
		{"multi-select", "喜欢甜的和酸的", "", true, []any{"喜欢甜的和酸的"}},
		{"number", "十二", "", false, float64(12)},
		{"number", "负十二点五", "", false, -12.5},
		{"number", "一百零二", "", false, float64(102)},
		{"number", "两万零五", "", false, float64(20005)},
		{"number", "+12.50", "", false, 12.5},
		{"date", "2024-02-29", "", false, "2024-02-29"},
		{"date", "二〇二六年十一月七日", "", false, "2026-11-07"},
		{"date", "明天", "Asia/Shanghai", false, "2026-11-08"},
		{"date", "明天", "America/Los_Angeles", false, "2026-11-07"},
		{"datetime", "明天下午三点", "Asia/Shanghai", false, "2026-11-08 15:00:00"},
		{"datetime", "后天上午十点半", "Asia/Shanghai", false, "2026-11-09 10:30:00"},
		{"datetime", "2026年11月7日23:15:45", "", false, "2026-11-07 23:15:45"},
		{"datetime", "2026-11-07T15:00", "", false, "2026-11-07T15:00"},
	} {
		t.Run(tt.kind+"/"+tt.speech+"/"+tt.zone, func(t *testing.T) {
			q := map[string]any{"id": "q", "type": tt.kind, "options": options, "allowFreeText": tt.free}
			got, err := convertQuestionAnswer(q, tt.speech, tt.zone, finalized)
			key := "answer"
			if tt.kind == "multi-select" {
				key = "answers"
			}
			if err != nil || got["id"] != "q" || !reflect.DeepEqual(got[key], tt.want) {
				t.Fatalf("got %#v, %v; want %#v", got, err, tt.want)
			}
		})
	}
}

func TestQuestionUnclearSpeechStaysUnanswered(t *testing.T) {
	for kind, texts := range map[string][]string{
		"number":       {"十二元", "一百二", "三万五", "十二或十三", "NaN", "Infinity", "1e999", "一点", "一百百"},
		"date":         {"明天", "2026-02-29", "2026年2月30日", "周末", "今天下午"},
		"datetime":     {"明天下午三点", "2026年11月7日三点", "2026年11月7日下午", "2026年11月7日25:00", "2026年11月7日下午三点六十分"},
		"select":       {"第四个", "第一和第三", "苹果和梨"},
		"multi-select": {"第一和第五", "苹果和梨", "第一个或第三个"},
		"password":     {"秘密"},
	} {
		for _, speech := range texts {
			t.Run(kind+"/"+speech, func(t *testing.T) {
				q := map[string]any{"id": "q", "type": kind, "allowFreeText": true, "options": []any{map[string]any{"label": "苹果"}, map[string]any{"label": "香蕉"}, map[string]any{"label": "梨子"}}}
				if got, err := convertQuestionAnswer(q, speech, "", time.Now()); err == nil {
					t.Fatalf("ambiguous speech accepted: %#v", got)
				}
			})
		}
	}
	q := map[string]any{"id": "q", "type": "select", "options": []any{map[string]any{"label": "相同"}, map[string]any{"label": "相同"}}}
	if _, err := convertQuestionAnswer(q, "相同", "", time.Now()); err == nil {
		t.Fatal("duplicate label accepted")
	}
}
