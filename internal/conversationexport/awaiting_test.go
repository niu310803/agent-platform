package conversationexport

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"agent-platform/internal/chat"
	"agent-platform/internal/stream"
)

func TestAwaitingAnswerDisplayProjection(t *testing.T) {
	for _, test := range []struct {
		name, mode, answer, definition, want string
	}{
		{"single", "question", `{"answers":[{"id":"q","answer":"a","question":"saved"}]}`, `{"questions":[{"id":"q","question":"original","options":[{"value":"a","label":"Choice A"}]}]}`, `{"status":"answered","items":[{"id":"q","question":"saved","answer":"Choice A"}]}`},
		{"multi", "question", `{"answers":[{"id":"q","answer":["a","b"]}]}`, `{"questions":[{"id":"q","question":"Choose","options":[{"value":"a","label":"A"}]}]}`, `{"status":"answered","items":[{"id":"q","question":"Choose","answers":["A","b"]}]}`},
		{"free text without metadata", "question", `{"answers":[{"id":"q","answer":"用户原文"}]}`, `{}`, `{"status":"answered","items":[{"id":"q","answer":"用户原文"}]}`},
		{"valid falsy values", "question", `{"answers":[{"id":"zero","answer":0},{"id":"false","answer":false},{"id":"empty","answer":""},{"id":"null","answer":null}]}`, `{}`, `{"status":"answered","items":[{"id":"zero","answer":0},{"id":"false","answer":false},{"id":"empty","answer":""},{"id":"null","answer":null}]}`},
		{"password", "question", `{"answers":[{"id":"q","answer":"secret","answers":["secret"]}]}`, `{"questions":[{"id":"q","type":"password"}]}`, `{"status":"answered","items":[{"id":"q","answer":"••••••","answers":["••••••"]}]}`},
		{"approval", "approval", `{"approvals":[{"id":"a","decision":"approve_rule_run","reason":"本次运行允许"}]}`, `{"approvals":[{"id":"a","command":"python3 probe.py","description":"像素扫描","policy":{"secret":"private"}}]}`, `{"status":"answered","items":[{"id":"a","decision":"approve_rule_run","reason":"本次运行允许","command":"python3 probe.py","description":"像素扫描"}]}`},
		{"form", "form", `{"form":{"decision":"approve","data":{"count":0,"enabled":false,"note":""}}}`, `{"form":{"title":"申请表","action":"submit"}}`, `{"status":"answered","items":[{"title":"申请表","action":"submit","decision":"approve","data":{"count":0,"enabled":false,"note":""}}]}`},
		{"planning", "planning", `{"planning":{"planningId":"p","decision":"reject","reason":"请补充"}}`, `{"planning":{"title":"计划"}}`, `{"status":"answered","planning":{"planningId":"p","title":"计划","decision":"reject","reason":"请补充"}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var answer, definition, want map[string]any
			for raw, target := range map[string]*map[string]any{test.answer: &answer, test.definition: &definition, test.want: &want} {
				if err := json.Unmarshal([]byte(raw), target); err != nil {
					t.Fatal(err)
				}
			}
			answer["mode"], answer["status"] = test.mode, "answered"
			text, err := projectAwaitingAnswer(stream.EventData{Payload: answer}, stream.EventData{Payload: definition}, "zh-CN")
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal([]byte(text), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("got %s; want %s", text, test.want)
			}
		})
	}
}

func TestAwaitingAnswersSurviveTerminalAndDeduplicatePerRun(t *testing.T) {
	events := []stream.EventData{}
	for i, runID := range []string{"one", "two"} {
		at := testEpoch + int64(i*10)
		events = append(events,
			stream.EventData{Type: "request.query", Timestamp: at, Payload: map[string]any{"runId": runID, "message": "question"}},
			stream.EventData{Type: "awaiting.ask", Timestamp: at, Payload: map[string]any{"runId": runID, "taskId": "task", "awaitingId": "same-id", "mode": "approval"}},
			stream.EventData{Type: "run.error", Timestamp: at + 1, Payload: map[string]any{"runId": runID}},
		)
		for j := range 2 {
			events = append(events, stream.EventData{Type: "awaiting.answer", Timestamp: at + 2 + int64(j), Payload: map[string]any{
				"runId": runID, "awaitingId": "same-id", "mode": "approval", "status": "answered",
				"approvals": []any{map[string]any{"id": "a", "decision": "approve"}},
			}})
		}
	}
	snapshot, err := buildSnapshotForTest(&chat.Summary{CreatedAt: testEpoch}, events, testEpoch+20)
	if err != nil {
		t.Fatal(err)
	}
	for i, turn := range snapshot.Turns {
		if len(turn.Nodes) != 2 || turn.Nodes[1].TaskID != "task" || turn.Nodes[1].At != testEpoch+int64(i*10)+3 || turn.Error == nil {
			t.Fatalf("invalid late answer projection: %+v", turn)
		}
	}
}

func TestMarkdownFailedCancelledAndRunningLocales(t *testing.T) {
	for _, locale := range []string{"zh-CN", "en-US"} {
		for _, outcome := range []Outcome{OutcomeFailed, OutcomeCancelled, OutcomeRunning} {
			for _, body := range []string{"", "partial answer"} {
				snapshot := SnapshotV1{Locale: locale, Turns: []TurnV1{{Outcome: outcome, Nodes: []NodeV1{
					{Kind: "message", Role: "user", Text: "question"},
					{Kind: "content", Role: "assistant", Text: body},
					{Kind: "awaiting-answer", Text: "private interaction"},
				}}}}
				markdown, err := RenderMarkdown(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				text := string(markdown)
				if strings.Contains(text, "private interaction") || (body != "" && !strings.Contains(text, body)) {
					t.Fatal(text)
				}
				if locale == "zh-CN" && !strings.Contains(text, "用户问题") || locale == "en-US" && !strings.Contains(text, "User question") {
					t.Fatal(text)
				}
				if body == "" && !strings.Contains(text, "未生成回答正文") && !strings.Contains(text, "No answer was generated") {
					t.Fatal(text)
				}
			}
		}
	}
	if _, err := RenderMarkdown(SnapshotV1{}); !errors.Is(err, ErrNoRootTurn) {
		t.Fatalf("err=%v", err)
	}
}
