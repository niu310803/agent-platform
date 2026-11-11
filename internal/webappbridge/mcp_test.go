package webappbridge

import (
	"agent-platform/internal/config"
	"agent-platform/internal/remotemcp"
	"agent-platform/internal/webapp"
	"agent-platform/internal/webappgateway"
	"context"
	"encoding/json"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMCPInvokeComposeScopeAndCapability(t *testing.T) {
	apps, e := fixture(t)
	upstream := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	upstream.AddTool(&sdk.Tool{Name: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: "ok"}}, IsError: true}, nil
	})
	remote := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, &sdk.StreamableHTTPOptions{JSONResponse: true}))
	defer remote.Close()
	bridge := New(Options{Apps: apps, Owner: "owner", RemoteMCP: &remotemcp.Service{}})
	defer bridge.Close()
	input := map[string]any{"url": remote.URL, "toolName": "x", "arguments": map[string]any{}, "auth": "none"}
	w := call(t, bridge, apps, e.ID, "mcp.invoke", input)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"mcp":`) || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = call(t, bridge, apps, e.ID, "desktop.capabilities.list", map[string]any{})
	if !strings.Contains(w.Body.String(), `"id":"mcp.invoke"`) {
		t.Fatal(w.Body.String())
	}
	input["token"] = "spoof"
	w = call(t, bridge, apps, e.ID, "mcp.invoke", input)
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	delete(input, "token")
	// Exercise the page gateway: an anonymous request cannot borrow owner SSO.
	cfg := config.DefaultWebAppsConfig()
	cfg.PlatformOrigin = "http://127.0.0.1:11948"
	gateway := webappgateway.New(apps, cfg)
	gateway.Actions = bridge
	defer gateway.Close()
	launch, err := gateway.Launch("owner", e.ID, "/")
	if err != nil {
		t.Fatal(err)
	}
	rawPage, _ := json.Marshal(map[string]any{"action": "mcp.invoke", "args": input})
	page := func(cookie *http.Cookie, csrf string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", gateway.Origin(e.ID)+"/__desktop/actions/call", strings.NewReader(string(rawPage)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", gateway.Origin(e.ID))
		r.Header.Set("X-WebApp-CSRF", csrf)
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		gateway.ServeHTTP(w, r)
		return w
	}
	if w := page(nil, ""); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	r := httptest.NewRequest("POST", launch.Action, strings.NewReader("ticket="+launch.Ticket))
	r.Header.Set("Origin", cfg.PlatformOrigin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	cookie := w.Result().Cookies()[0]
	r = httptest.NewRequest("GET", gateway.Origin(e.ID)+"/__desktop/bootstrap.json", nil)
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	gateway.ServeHTTP(w, r)
	var boot struct {
		Token string `json:"csrfToken"`
	}
	json.Unmarshal(w.Body.Bytes(), &boot)
	if w := page(cookie, ""); w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := page(cookie, boot.Token); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Real backend bootstrap/action credentials exercise the same dispatch without a Desktop identity.
	if err := bridge.ListenInternal(); err != nil {
		t.Fatal(err)
	}
	scope, err := apps.Acquire(e.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer scope.Release()
	spec := webapp.RuntimeSpec{ID: e.ID, Generation: scope.Generation, Policy: scope.Policy, Context: scope.Context, Check: scope.Check}
	url, token, revoke, err := bridge.GrantBackend(spec)
	if err != nil {
		t.Fatal(err)
	}
	defer revoke()
	req, _ := http.NewRequest("POST", url+"/actions/call", strings.NewReader(`{"action":"mcp.invoke","args":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	// Bootstrap credentials must not be usable as action credentials.
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	req, _ = http.NewRequest("POST", url+"/session", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var session struct {
		Token string `json:"token"`
	}
	json.NewDecoder(res.Body).Decode(&session)
	res.Body.Close()
	if res.StatusCode != 200 || session.Token == "" {
		t.Fatal(res.StatusCode)
	}
	actionToken := session.Token
	raw, _ := json.Marshal(map[string]any{"action": "mcp.invoke", "args": input})
	req, _ = http.NewRequest("POST", url+"/actions/call", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+actionToken)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatal(res.StatusCode)
	}
	revoke()
	req, _ = http.NewRequest("POST", url+"/actions/call", strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+actionToken)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal(res.StatusCode)
	}
	bridge.owner = ""
	w = call(t, bridge, apps, e.ID, "mcp.invoke", input)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
}
