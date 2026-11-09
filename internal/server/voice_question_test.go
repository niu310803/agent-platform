package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	rt "agent-platform/internal/runtime/types"
	"agent-platform/internal/voice"
	"github.com/gorilla/websocket"
)

type questionSubmitRuntime struct {
	voice.Runtime
	commands chan rt.SubmitCommand
	unknown  bool
}

func (r questionSubmitRuntime) Submit(ctx context.Context, command rt.SubmitCommand) (rt.SubmitResult, error) {
	r.commands <- command
	if r.unknown {
		return rt.SubmitResult{}, errors.New("injected unknown outcome")
	}
	return r.Runtime.Submit(ctx, command)
}

func TestVoiceQuestionUIWinsOverLateASR(t *testing.T) {
	var calls atomic.Int32
	f := newTestFixtureWithModelHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeProviderSSE(t, w, providerToolCallsFrame(t, []providerToolCallSpec{{ID: "question", Name: "ask_user_question", Args: map[string]any{"mode": "question", "questions": []any{map[string]any{"id": "text", "type": "text", "question": "如何继续"}}}}}), "[DONE]")
		} else {
			writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"UI 已完成。"},"finish_reason":"stop"}]}`, "[DONE]")
		}
	})
	commands := make(chan rt.SubmitCommand, 2)
	srv, p := enableVoiceFixture(t, f, questionSubmitRuntime{Runtime: f.server.deps.Runtime.(voice.Runtime), commands: commands})
	c, chatID := dialVoice(t, srv, "", 16000)
	speakVoice(t, c)
	ask, _ := voiceUntil(t, c, "awaiting", "ask")
	voiceUntil(t, c, "tts", "stop")
	p.transcript.Store("late-asr-never-submitted")
	startQuestionVoice(t, c, ask)
	if err := c.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"agentKey": "mock-agent", "runId": ask["runId"], "awaitingId": ask["awaitingId"], "submitId": "ui-wins", "params": []any{map[string]any{"id": "text", "answer": "ui-first"}}})
	rec := httptest.NewRecorder()
	f.server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatal(rec.Code, rec.Body.String())
	}
	voiceUntil(t, c, "awaiting", "resolved")
	final, _ := voiceUntil(t, c, "tts", "start")
	if final["purpose"] != "final" || final["text"] != "UI 已完成。" {
		t.Fatal(final)
	}
	voiceUntil(t, c, "tts", "stop")
	if len(commands) != 0 || len(queryLines(t, f, chatID)) != 1 {
		t.Fatal("late voice answered or opened Query")
	}
	data, err := os.ReadFile(filepath.Join(f.cfg.Paths.ChatsDir, chatID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("late-asr-never-submitted")) {
		t.Fatal("late transcript persisted")
	}
}

func TestVoiceQuestionUnknownSubmitKeepsDraftAndDoesNotRetry(t *testing.T) {
	f := newTestFixtureWithModelHandler(t, func(w http.ResponseWriter, r *http.Request) {
		writeProviderSSE(t, w, providerToolCallsFrame(t, []providerToolCallSpec{{ID: "question", Name: "ask_user_question", Args: map[string]any{"mode": "question", "questions": []any{map[string]any{"id": "text", "type": "text", "question": "如何继续"}}}}}), "[DONE]")
	})
	commands := make(chan rt.SubmitCommand, 4)
	srv, p := enableVoiceFixture(t, f, questionSubmitRuntime{Runtime: f.server.deps.Runtime.(voice.Runtime), commands: commands, unknown: true})
	c, chatID := dialVoice(t, srv, "", 16000)
	speakVoice(t, c)
	ask, _ := voiceUntil(t, c, "awaiting", "ask")
	voiceUntil(t, c, "tts", "stop")
	answerVoiceQuestion(t, c, p, ask, "保留草稿")
	for {
		frame, _ := readVoice(t, c)
		if frame["type"] == "error" {
			if frame["code"] != "question_failed" {
				t.Fatal(frame)
			}
			break
		}
	}
	detail, err := f.server.loadChatDetail(context.Background(), chatID, false)
	if err != nil || detail.Awaiting == nil || detail.Awaiting.QuestionDraft == nil || detail.Awaiting.QuestionDraft.Revision != 1 {
		t.Fatal(detail.Awaiting, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, _ := dialVoice(t, srv, chatID, 16000)
	if err := c2.WriteJSON(map[string]any{"type": "abort"}); err != nil {
		t.Fatal(err)
	}
	voiceUntil(t, c2, "abort", "")
	if len(commands) != 1 || len(queryLines(t, f, chatID)) != 1 {
		t.Fatal("unknown Submit replayed")
	}
}

func startQuestionVoice(t *testing.T, c *websocket.Conn, awaiting map[string]any) {
	t.Helper()
	previous, _ := voiceTurns.Load(c)
	turn := previous.(int) + 1
	voiceTurns.Store(c, turn)
	if err := c.WriteJSON(map[string]any{"type": "listen", "state": "start", "turn": turn, "requestId": fmt.Sprintf("listen-%d", turn), "intent": "question", "runId": awaiting["runId"], "awaitingId": awaiting["awaitingId"], "questionId": awaiting["questionId"]}); err != nil {
		t.Fatal(err)
	}
	ready, _ := voiceUntil(t, c, "listen.ready", "")
	if ready["turn"] != float64(turn) {
		t.Fatal(ready)
	}
}

func answerVoiceQuestion(t *testing.T, c *websocket.Conn, p *voiceTestProvider, awaiting map[string]any, text string) {
	t.Helper()
	p.transcript.Store(text)
	startQuestionVoice(t, c, awaiting)
	if err := c.WriteMessage(websocket.BinaryMessage, make([]byte, 640)); err != nil {
		t.Fatal(err)
	}
	if err := stopVoice(c); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceQuestionAllTypesSubmitOnceOrHandOffPassword(t *testing.T) {
	for _, password := range []bool{false, true} {
		t.Run(fmt.Sprint(password), func(t *testing.T) {
			questions := []any{
				map[string]any{"id": "text", "question": "请说内容", "type": "text"},
				map[string]any{"id": "select", "question": "请选择", "type": "select", "options": []any{map[string]any{"label": "苹果"}, map[string]any{"label": "香蕉"}}},
				map[string]any{"id": "multi", "question": "请多选", "type": "multi-select", "options": []any{map[string]any{"label": "苹果"}, map[string]any{"label": "香蕉"}, map[string]any{"label": "梨"}}},
				map[string]any{"id": "number", "question": "请说数量", "type": "number"},
				map[string]any{"id": "date", "question": "请说日期", "type": "date"},
				map[string]any{"id": "datetime", "question": "请说时间", "type": "datetime"},
			}
			if password {
				questions = append([]any{map[string]any{"id": "password", "question": "只在界面填密码", "type": "password"}}, questions...)
			}
			var calls atomic.Int32
			f := newTestFixtureWithModelHandler(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					writeProviderSSE(t, w, providerToolCallsFrame(t, []providerToolCallSpec{{ID: "question-tool", Name: "ask_user_question", Args: map[string]any{"mode": "question", "questions": questions}}}), "[DONE]")
				} else {
					writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"所有问题完成。"},"finish_reason":"stop"}]}`, "[DONE]")
				}
			})
			commands := make(chan rt.SubmitCommand, 4)
			srv, p := enableVoiceFixture(t, f, questionSubmitRuntime{Runtime: f.server.deps.Runtime.(voice.Runtime), commands: commands})
			p.reportUsage.Store(true)
			c, chatID := dialVoice(t, srv, "", 16000)
			speakVoice(t, c)
			awaiting, _ := voiceUntil(t, c, "awaiting", "ask")
			texts := []string{"保留的普通答案", "第二个", "第一和第三", "十二", "2028-02-29", "明天下午三点"}
			ids := []string{"text", "select", "multi", "number", "date", "datetime"}
			for i, text := range texts {
				if awaiting["questionId"] != ids[i] || awaiting["handoff"] != false {
					t.Fatal("out of order", awaiting)
				}
				start, _ := voiceUntil(t, c, "tts", "start")
				if start["purpose"] != "question" || strings.Contains(start["text"].(string), "密码") {
					t.Fatal(start)
				}
				voiceUntil(t, c, "tts", "stop")
				if i == 3 {
					answerVoiceQuestion(t, c, p, awaiting, "一百二")
					awaiting, _ = voiceUntil(t, c, "awaiting", "ask")
					if awaiting["questionId"] != "number" {
						t.Fatal("ambiguous answer advanced", awaiting)
					}
					retry, _ := voiceUntil(t, c, "tts", "start")
					if !strings.HasPrefix(retry["text"].(string), "答案不够明确") {
						t.Fatal(retry)
					}
					voiceUntil(t, c, "tts", "stop")
				}
				answerVoiceQuestion(t, c, p, awaiting, text)
				if i < len(texts)-1 {
					awaiting, _ = voiceUntil(t, c, "awaiting", "ask")
				}
			}
			if password {
				handoff, _ := voiceUntil(t, c, "awaiting", "ask")
				if handoff["handoff"] != true || handoff["questionId"] != "" {
					t.Fatal(handoff)
				}
				voiceUntil(t, c, "tts", "stop")
				if len(commands) != 0 || calls.Load() != 1 {
					t.Fatal("password group auto-submitted")
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				detail, err := f.server.loadChatDetail(context.Background(), chatID, false)
				if err != nil || detail.Awaiting == nil || detail.Awaiting.QuestionDraft == nil || detail.Awaiting.QuestionDraft.Revision != 6 {
					t.Fatal("draft not readable after disconnect", detail.Awaiting, err)
				}
				draft := detail.Awaiting.QuestionDraft
				if len(draft.Answers) != 6 || draft.Answers[0]["answer"] != "保留的普通答案" {
					t.Fatal(draft)
				}
				data, err := os.ReadFile(filepath.Join(f.cfg.Paths.ChatsDir, chatID+".jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(data, []byte("保留的普通答案")) || bytes.Contains(data, []byte("questionDraft")) {
					t.Fatal("draft persisted before Submit")
				}
				params := append([]map[string]any{{"id": "password", "answer": "ui-only-secret"}}, draft.Answers...)
				body, _ := json.Marshal(map[string]any{"agentKey": "mock-agent", "runId": awaiting["runId"], "awaitingId": awaiting["awaitingId"], "submitId": "ui-password", "params": params})
				rec := httptest.NewRecorder()
				f.server.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/submit", bytes.NewReader(body)))
				if rec.Code != http.StatusOK {
					t.Fatal(rec.Code, rec.Body.String())
				}
				deadline := time.Now().Add(5 * time.Second)
				for {
					summary, _ := f.chats.Summary(chatID)
					if summary.LastRunContent == "所有问题完成。" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("UI continuation not persisted")
					}
					time.Sleep(5 * time.Millisecond)
				}
			} else {
				final, _ := voiceUntil(t, c, "tts", "start")
				if final["purpose"] != "final" || final["text"] != "所有问题完成。" {
					t.Fatal(final)
				}
				voiceUntil(t, c, "tts", "stop")
				if len(commands) != 1 {
					t.Fatal("Submit count", len(commands))
				}
				cmd := <-commands
				if cmd.SubmitID == "" || len(cmd.Params) != 6 {
					t.Fatal(cmd)
				}
				var number, mapMulti map[string]any
				json.Unmarshal(cmd.Params[3], &number)
				json.Unmarshal(cmd.Params[2], &mapMulti)
				if number["answer"] != float64(12) || !reflect.DeepEqual(mapMulti["answers"], []any{"苹果", "梨"}) {
					t.Fatal(number, mapMulti)
				}
			}
			if calls.Load() != 2 || len(queryLines(t, f, chatID)) != 1 {
				t.Fatal("question flow opened another Query", calls.Load())
			}
			detail, err := f.server.loadChatDetail(context.Background(), chatID, false)
			if err != nil || detail.Usage == nil || detail.Usage.Chat.Voice == nil {
				t.Fatal("question usage missing", err)
			}
			usage := detail.Usage.Chat.Voice
			if usage.Tasks != 16 || usage.BilledAudioSeconds != 24 || usage.Characters != 96 || usage.UnknownTasks != 0 {
				t.Fatalf("question/retry/handoff usage = %+v", usage)
			}
			if detail.Usage.LastRun.Voice.Tasks != 15 {
				t.Fatal("invalid answer ASR should remain Chat-only", detail.Usage.LastRun.Voice)
			}
		})
	}
}
