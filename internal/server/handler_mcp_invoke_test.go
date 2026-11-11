package server

import (
	"agent-platform/internal/remotemcp"
	"context"
	"encoding/json"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPInvokeAuthorityAndStrictInput(t *testing.T) {
	s := &Server{deps: Dependencies{RemoteMCP: &remotemcp.Service{}}}
	for _, p := range []*Principal{nil, {Subject: "", Claims: map[string]any{"scope": "app", "deviceId": "d"}}, {Subject: "owner", Claims: map[string]any{"scope": "web", "deviceId": "d"}}, {Subject: "owner", Claims: map[string]any{"scope": "app"}}} {
		r := httptest.NewRequest("POST", "/api/mcp/invoke", strings.NewReader(`{}`))
		r = r.WithContext(WithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		s.handleMCPInvoke(w, r)
		if w.Code != 403 || !strings.Contains(w.Body.String(), "mcp_authority_required") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	p := &Principal{Subject: "host", Claims: map[string]any{"scope": "app", "deviceId": "d"}}
	for _, raw := range []string{`{"url":"https://host","toolName":"x","arguments":{},"headers":{}}`, `{"url":"https://host","toolName":"x","arguments":{},"subject":"owner"}`} {
		r := httptest.NewRequest("POST", "/api/mcp/invoke", strings.NewReader(raw))
		r = r.WithContext(WithPrincipal(r.Context(), p))
		w := httptest.NewRecorder()
		s.handleMCPInvoke(w, r)
		if w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// This route authenticates even when general API authentication is disabled.
	w := httptest.NewRecorder()
	if r := s.withPrincipal(httptest.NewRequest("POST", "/api/mcp/invoke", nil), w); r != nil || w.Code != 401 || !strings.Contains(w.Body.String(), "errorCode") {
		t.Fatal(r, w.Code, w.Body.String())
	}
}
func TestMCPInvokeHTTPResult(t *testing.T) {
	upstream := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	upstream.AddTool(&sdk.Tool{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}, StructuredContent: map[string]any{"x": 1}, IsError: true}, nil
	})
	remote := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, &sdk.StreamableHTTPOptions{JSONResponse: true}))
	defer remote.Close()
	s := &Server{deps: Dependencies{RemoteMCP: &remotemcp.Service{}}}
	raw := `{"url":"` + remote.URL + `","toolName":"x","arguments":{},"auth":"none"}`
	r := httptest.NewRequest("POST", "/api/mcp/invoke", strings.NewReader(raw))
	r = r.WithContext(WithPrincipal(r.Context(), &Principal{Subject: "host", Claims: map[string]any{"scope": "app", "device_id": "d"}}))
	w := httptest.NewRecorder()
	s.handleMCPInvoke(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mcp":`) || !strings.Contains(w.Body.String(), `"isError":true`) || !strings.Contains(w.Body.String(), `"structuredContent":`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
