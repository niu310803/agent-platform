package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"agent-platform/internal/mailbox"
)

// /api/query and /api/steer pass through mailbox but keep their contracts: a
// busy Chat rejects the query, and the steer joins the running Run under the
// caller's steer identity.
func TestInteractiveQueryAndSteerKeepTheirContractsThroughMailbox(t *testing.T) {
	const (
		chatID  = "chat-interactive-input"
		steerID = "client-steer-1"
	)
	var calls atomic.Int32
	releaseFirst := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFirst) }) }
	t.Cleanup(release)
	fixture := newTestFixtureWithModelHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"draft answer"}}]}`)
			<-releaseFirst
			writeProviderSSE(t, w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`)
		default:
			writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"steer handled"},"finish_reason":"stop"}]}`, `[DONE]`)
		}
	})
	httpServer := newLoopbackServer(t, fixture.server)
	defer httpServer.Close()

	resp, err := http.Post(httpServer.URL+"/api/query", "application/json", bytes.NewBufferString(`{"chatId":"`+chatID+`","message":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var stream strings.Builder
	runID := ""
	for {
		line, readErr := reader.ReadString('\n')
		stream.WriteString(line)
		if strings.HasPrefix(line, "data: {") {
			payload := decodeSSELine(t, line)
			if id, _ := payload["runId"].(string); id != "" {
				runID = id
			}
			if payload["type"] == "content.delta" && payload["delta"] == "draft answer" {
				break
			}
		}
		if readErr != nil {
			t.Fatalf("read stream: %v %s", readErr, stream.String())
		}
	}

	busy := httptest.NewRecorder()
	busyReq := httptest.NewRequest(http.MethodPost, "/api/query", bytes.NewBufferString(`{"chatId":"`+chatID+`","message":"second","requestId":"busy-query"}`))
	busyReq.Header.Set("Content-Type", "application/json")
	fixture.server.ServeHTTP(busy, busyReq)
	if busy.Code != http.StatusConflict || !strings.Contains(busy.Body.String(), "active_run_conflict") {
		t.Fatalf("busy query: %d %s", busy.Code, busy.Body.String())
	}
	if _, err := fixture.server.deps.Mailbox.Store.UserRequest("", "busy-query"); err != mailbox.ErrNotFound {
		t.Fatalf("rejected query was stored: %v", err)
	}

	steer := httptest.NewRecorder()
	steerReq := httptest.NewRequest(http.MethodPost, "/api/steer", bytes.NewBufferString(`{"agentKey":"mock-agent","runId":"`+runID+`","chatId":"`+chatID+`","message":"focus","steerId":"`+steerID+`","requestId":"steer-request"}`))
	steerReq.Header.Set("Content-Type", "application/json")
	fixture.server.ServeHTTP(steer, steerReq)
	var ack struct {
		Data struct {
			Accepted bool   `json:"accepted"`
			Status   string `json:"status"`
			SteerID  string `json:"steerId"`
			RunID    string `json:"runId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(steer.Body.Bytes(), &ack); err != nil || steer.Code != http.StatusOK || !ack.Data.Accepted || ack.Data.Status != "accepted" || ack.Data.SteerID != steerID || ack.Data.RunID != runID {
		t.Fatalf("steer ack: %d %s %v", steer.Code, steer.Body.String(), err)
	}

	release()
	steerEvent := map[string]any{}
	for {
		line, readErr := reader.ReadString('\n')
		stream.WriteString(line)
		if strings.HasPrefix(line, "data: {") {
			if payload := decodeSSELine(t, line); payload["type"] == "request.steer" {
				steerEvent = payload
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
	}
	if steerEvent["steerId"] != steerID || steerEvent["requestId"] != "steer-request" {
		t.Fatalf("request.steer identity: %#v", steerEvent)
	}
	mail, err := fixture.server.deps.Mailbox.Store.UserRequest("", "steer-request")
	if err != nil || !mail.Immediate || mail.SteerID != steerID || mail.RunID != runID || mail.ConsumedAt == 0 {
		t.Fatalf("steer mail: %+v %v", mail, err)
	}
	content, err := fixture.chats.LoadJSONLContent(chatID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, `"steerId":"`+steerID+`"`) {
		t.Fatalf("persisted steer lost the caller identity: %s", content)
	}
}
