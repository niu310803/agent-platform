package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestOperationHTTPUsesManagedOneID(t *testing.T) {
	var expected atomic.Value
	expected.Store("identity-a")
	var calls atomic.Int32
	upstream := sdk.NewServer(&sdk.Implementation{Name: "oneid-operation", Version: "1"}, nil)
	upstream.AddTool(&sdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		calls.Add(1)
		return &sdk.CallToolResult{StructuredContent: map[string]any{"ok": true}}, nil
	})
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return upstream }, &sdk.StreamableHTTPOptions{JSONResponse: true, Stateless: true})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+expected.Load().(string) || r.Header.Get("X-Identity") != expected.Load().(string) {
			t.Error("incorrect managed identity")
		}
		handler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	target, _ := url.Parse(endpoint.URL)
	pkg := writeOneIDFixture(t, t.TempDir(), map[string]any{"type": "streamableHttp", "url": "https://example.test/mcp", "headers": map[string]any{"X-Identity": "${AP_ID_TOKEN}"}})
	file := filepath.Join(t.TempDir(), "identity")
	client, key, err := NewOperationClient(pkg, "main", "read", file)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Keep the production HTTPS/destination guard; route its accepted requests to
	// the local fixture only in the underlying test transport.
	client.httpClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		cloned := r.Clone(r.Context())
		u := *r.URL
		u.Scheme, u.Host = target.Scheme, target.Host
		cloned.URL = &u
		cloned.Host = target.Host
		return endpoint.Client().Transport.RoundTrip(cloned)
	})}
	for _, token := range []string{"identity-a", "identity-b"} {
		expected.Store(token)
		if err := os.WriteFile(file, []byte(token), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := client.CallTool(t.Context(), key, "read", map[string]any{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CallTool(t.Context(), key, "read", map[string]any{}, nil); err == nil {
		t.Fatal("logout allowed a new operation")
	}
	if calls.Load() != 2 {
		t.Fatalf("business calls = %d", calls.Load())
	}
}
