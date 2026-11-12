package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-platform/internal/chatresource"
	rt "agent-platform/internal/runtime/types"
	"agent-platform/internal/webapp"
	"agent-platform/internal/webappbridge"
	"agent-platform/internal/webapptest"
)

func sdkApps(t *testing.T) (*webapp.Service, webapp.Entry) {
	t.Helper()
	apps, e, _ := webapptest.Open(t, `{"schemaVersion":2,"id":"webapp-0123456789abcdef","key":"sdk-test","label":"SDK","version":"1.0.0","target":"any","hosting":"platform","minPlatformVersion":"0.5.0","copilot":{"agentKey":"`+executeTestAgent+`","references":[{"type":"skill","id":"mock-skill"}]},"frontend":{"root":"frontend","index":"index.html"}}`, map[string]string{"frontend/index.html": "SDK", "agents/" + executeTestAgent + "/agent.yml": "key: " + executeTestAgent})
	return apps, e
}
func sdkAdapters(t *testing.T, f testFixture, apps *webapp.Service) *webappbridge.Adapters {
	t.Helper()
	return webappbridge.NewAdapters(webappbridge.AdapterOptions{Apps: apps, Runtime: f.server.deps.Runtime.(webappbridge.Runtime), Registry: f.registry, Chats: f.chats, Artifacts: chatresource.NewService(f.chats), Owner: "owner", Root: filepath.Join(t.TempDir(), "sdk")})
}
func sdkScope(t *testing.T, apps *webapp.Service, e webapp.Entry) *webapp.Scope {
	t.Helper()
	s, err := apps.Acquire(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Release)
	return s
}
func TestWebappSDKUsesRealExecuteAndFailClosedOwnership(t *testing.T) {
	f, calls := newExecuteFixture(t, func(call int32, r executeModelRequest, w http.ResponseWriter) {
		if call%2 == 1 {
			writeProviderSSE(t, w, submitFrame(t, "sdk_submit", map[string]any{"status": "success", "output": map[string]any{"summary": "SDK result"}}), `[DONE]`)
		} else {
			writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`, `[DONE]`)
		}
	})
	apps, e := sdkApps(t)
	scope := sdkScope(t, apps, e)
	adapters := sdkAdapters(t, f, apps)
	actions := adapters.Actions()
	raw, _ := json.Marshal(map[string]any{"message": "summarize", "input": map[string]any{"customer": "VIP"}, "outputSchema": executeTestSchema})
	result, err := actions["assistant.execute"](context.Background(), scope, raw)
	if err != nil {
		t.Fatal(err)
	}
	value, ok := result.(rt.ExecuteResult)
	if !ok || value.Status != "success" || value.Output["summary"] != "SDK result" {
		t.Fatalf("result=%#v", result)
	}
	items, err := actions["artifact.list"](context.Background(), scope, json.RawMessage(`{"chatId":"`+value.ChatID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if items == nil {
		t.Fatal("missing result")
	}
	other := *scope
	other.InstallationID = "different-installation"
	if _, err = actions["artifact.list"](context.Background(), &other, json.RawMessage(`{"chatId":"`+value.ChatID+`"}`)); err == nil {
		t.Fatal("cross-app chat accepted")
	}
	count := calls.Load()
	_, err = f.server.deps.Runtime.Execute(context.Background(), rt.ExecuteCommand{AgentKey: executeTestAgent, Message: "must not execute", OutputSchema: executeTestSchema, AccessLevel: "ask_for_approval", OnPrepared: func(rt.RunRef) error { return errors.New("ledger unavailable") }})
	if err == nil || !strings.Contains(err.Error(), "ledger unavailable") || calls.Load() != count {
		t.Fatalf("failed claim started model: %v", err)
	}
}
func TestWebappSDKBackgroundChatEventsAndGenerationCancellation(t *testing.T) {
	started := make(chan struct{}, 1)
	f, _ := newExecuteFixture(t, func(call int32, r executeModelRequest, w http.ResponseWriter) {
		started <- struct{}{}
		writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"hello SDK"}}]}`)
		// Finish normally; a second invocation below checks generation revocation.
		writeProviderSSE(t, w, `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, `[DONE]`)
	})
	apps, e := sdkApps(t)
	scope := sdkScope(t, apps, e)
	actions := sdkAdapters(t, f, apps).Actions()
	result, err := actions["assistant.chat"](context.Background(), scope, json.RawMessage(`{"message":"hello","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	value := result.(map[string]any)
	runID := value["runId"].(string)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model not called")
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		events, err := actions["assistant.events"](context.Background(), scope, json.RawMessage(`{"runId":"`+runID+`"}`))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(events)
		if events.(map[string]any)["terminal"] == true {
			if !bytes.Contains(raw, []byte("hello SDK")) {
				t.Fatalf("text missing: %s", raw)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not terminal: %s", raw)
		}
		time.Sleep(50 * time.Millisecond)
	}
	other := *scope
	other.Generation = "other-generation"
	if _, err = actions["assistant.events"](context.Background(), &other, json.RawMessage(`{"runId":"`+runID+`"}`)); err == nil {
		t.Fatal("old generation accepted")
	}
	scope.Release()
	_, err = apps.Stop(context.Background(), e.ID, webapp.StateRequest{ExpectedRevision: e.Revision, ExpectedStateRevision: e.StateRevision})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWebappSDKStopDrainsActiveAssistant(t *testing.T) {
	releaseModel := make(chan struct{})
	defer close(releaseModel)
	started := make(chan struct{}, 1)
	f, _ := newExecuteFixture(t, func(call int32, request executeModelRequest, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"waiting\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-releaseModel
	})
	apps, e := sdkApps(t)
	scope := sdkScope(t, apps, e)
	actions := sdkAdapters(t, f, apps).Actions()
	result, err := actions["assistant.chat"](context.Background(), scope, json.RawMessage(`{"message":"wait until stopped","background":true}`))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model not called")
	}
	scope.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = apps.Stop(ctx, e.ID, webapp.StateRequest{ExpectedRevision: e.Revision, ExpectedStateRevision: e.StateRevision}); err != nil {
		t.Fatal("generation could not drain", err)
	}
	runID := result.(map[string]any)["runId"].(string)
	snapshot, err := f.server.deps.Runtime.GetRunStatus(runID)
	if err != nil || snapshot.CompletedAt == 0 {
		t.Fatalf("run still active: %#v %v", snapshot, err)
	}
}
