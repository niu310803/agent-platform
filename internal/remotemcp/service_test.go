package remotemcp

import (
	"context"
	"encoding/json"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T, tls bool, handler sdk.ToolHandler) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(ctx context.Context, r *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.Add(1)
		return handler(ctx, r)
	})
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true})
	var s *httptest.Server
	if tls {
		s = httptest.NewTLSServer(h)
	} else {
		s = httptest.NewServer(h)
	}
	t.Cleanup(s.Close)
	return s, calls
}
func success(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "done"}}, StructuredContent: map[string]any{"ok": true}, IsError: true}, nil
}
func TestInvokeNativeResultAndNoRetry(t *testing.T) {
	upstream, calls := fixture(t, false, success)
	s := &Service{}
	result, err := s.Invoke(t.Context(), Request{URL: upstream.URL, ToolName: "write", Arguments: map[string]any{"nested": map[string]any{"x": 1}}, Auth: "none"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.MCP.IsError || len(result.MCP.Content) != 1 || result.MCP.StructuredContent == nil || calls.Load() != 1 {
		t.Fatalf("%+v calls %d", result, calls.Load())
	}
}
func TestSSORefreshAndMissing(t *testing.T) {
	var want atomic.Value
	want.Store("first-secret")
	server := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}, success)
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{JSONResponse: true})
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+want.Load().(string) {
			t.Error("identity header mismatch")
		}
		h.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	file := filepath.Join(t.TempDir(), "identity")
	s := &Service{SsoIDTokenFile: file, HTTPClient: upstream.Client()}
	req := Request{URL: upstream.URL, ToolName: "write", Arguments: map[string]any{}}
	for _, token := range []string{"first-secret", "second-secret"} {
		want.Store(token)
		os.WriteFile(file, []byte(token), 0600)
		if _, err := s.Invoke(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(file)
	_, err := s.Invoke(t.Context(), req)
	if code, _ := ErrorCode(err); code != "sso_identity_unavailable" {
		t.Fatal(err)
	}
}
func TestRedirectNeverForwardsIdentity(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached.Add(1) }))
	defer target.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 307) }))
	defer source.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("private"), 0600)
	_, err := (&Service{SsoIDTokenFile: file, HTTPClient: source.Client()}).Invoke(t.Context(), Request{URL: source.URL, ToolName: "write", Arguments: map[string]any{}})
	if err == nil || reached.Load() != 0 || strings.Contains(err.Error(), "private") {
		t.Fatal(err, reached.Load())
	}
}
func TestDeadlineCancellationAndLimits(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[cancel], func(t *testing.T) {
			upstream, calls := fixture(t, false, func(ctx context.Context, _ *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			})
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			if cancel {
				go func() {
					for calls.Load() == 0 {
						time.Sleep(time.Millisecond)
					}
					stop()
				}()
			}
			_, err := (&Service{Timeout: 100 * time.Millisecond}).Invoke(ctx, Request{URL: upstream.URL, ToolName: "write", Arguments: map[string]any{}, Auth: "none"})
			want := "mcp_timeout"
			if cancel {
				want = "mcp_canceled"
			}
			if code, _ := ErrorCode(err); code != want {
				t.Fatal(err)
			}
			if calls.Load() != 1 {
				t.Fatal(calls.Load())
			}
		})
	}
	upstream, _ := fixture(t, false, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: strings.Repeat("x", MaxOutput+1)}}}, nil
	})
	_, err := (&Service{}).Invoke(t.Context(), Request{URL: upstream.URL, ToolName: "write", Arguments: map[string]any{}, Auth: "none"})
	if code, _ := ErrorCode(err); code != "mcp_result_too_large" {
		t.Fatal(err)
	}
}
func TestDecodeStrict(t *testing.T) {
	for _, raw := range []string{
		`{"url":"http://localhost","toolName":"x","arguments":{}}`,
		`{"url":"https://host","toolName":"x","arguments":{},"headers":{}}`,
		`{"url":"https://host","toolName":"x","arguments":{},"token":"x"}`,
		`{"url":"https://host","URL":"https://host","toolName":"x","arguments":{}}`,
		`{"url":"https://host","url":"https://host","toolName":"x","arguments":{}}`,
		`{"url":"https://user:pass@host","toolName":"x","arguments":{}}`,
		`{"url":"https://host","toolName":"x","arguments":null}`,
		`{"url":"https://host","toolName":"x","arguments":{},"auth":null}`,
		`{"url":"https://host","toolName":"x","arguments":{}} {}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := Decode([]byte(`{"url":"http://127.0.0.1","toolName":"x","arguments":{},"auth":"none"}`)); err != nil {
		t.Fatal(err)
	}
}
func TestDroppedCallOutcomeUnknownNoRetry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			ID     any    `json:"id"`
			Method string `json:"method"`
		}
		json.Unmarshal(raw, &req)
		if req.Method == "initialize" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{}, "serverInfo": map[string]string{"name": "fixture", "version": "1"}}})
			return
		}
		if req.Method == "tools/call" {
			calls.Add(1)
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(202)
	}))
	defer server.Close()
	_, err := (&Service{}).Invoke(t.Context(), Request{URL: server.URL, ToolName: "write", Arguments: map[string]any{}, Auth: "none"})
	if code, _ := ErrorCode(err); code != "mcp_outcome_unknown" || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
}

func TestDecodePreservesArgumentNumbers(t *testing.T) {
	r, err := Decode([]byte(`{"url":"https://host/mcp","toolName":"x","arguments":{"id":9007199254740993}}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r.Arguments)
	if err != nil || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatal(string(raw), err)
	}
}
func TestStreamableSSEResponse(t *testing.T) {
	upstream := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	upstream.AddTool(&sdk.Tool{Name: "write", InputSchema: json.RawMessage(`{"type":"object"}`)}, success)
	server := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, nil))
	defer server.Close()
	result, err := (&Service{}).Invoke(t.Context(), Request{URL: server.URL, ToolName: "write", Arguments: map[string]any{}, Auth: "none"})
	if err != nil || result.MCP == nil {
		t.Fatal(result, err)
	}
}
