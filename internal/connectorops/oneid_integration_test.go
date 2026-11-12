package connectorops_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-platform/internal/connector"
	"agent-platform/internal/connectorauth"
	"agent-platform/internal/connectorops"
	"agent-platform/internal/view"
	"agent-platform/internal/viewaction"
	"agent-platform/internal/viewquery"
	"agent-platform/internal/webapp"
	"agent-platform/internal/webappbridge"
	"agent-platform/internal/webapptest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run a real native CLI or stdio MCP on both macOS and Windows. Return only
// whether the supplied identity matched, never the credential itself.
func TestOneIDExecutionHelper(t *testing.T) {
	mode := os.Getenv("ONEID_EXECUTION_HELPER")
	if mode == "" {
		return
	}
	identity := os.Getenv("AP_ID_TOKEN")
	result := map[string]any{"generation": map[string]string{"identity-a": "a", "identity-b": "b"}[identity], "matches": identity != "" && (os.Getenv("ONEID_REQUIRE_MAPPING") != "1" || os.Getenv("SERVICE_TOKEN") == identity)}
	if mode == "cli" {
		json.NewEncoder(os.Stdout).Encode(result)
		os.Exit(0)
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "oneid", Version: "1"}, nil)
	server.AddTool(&sdk.Tool{Name: "read", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return &sdk.CallToolResult{StructuredContent: result}, nil
	})
	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestOneIDGenericAndVIEWExecution(t *testing.T) {
	t.Setenv("AP_ID_TOKEN", "ambient-spoof")
	for _, mode := range []string{"cli", "cli-template", "stdio"} {
		t.Run(mode, func(t *testing.T) {
			sources := connector.Sources{ExternalRoot: t.TempDir(), StateRoot: t.TempDir()}
			dir := filepath.Join(sources.ExternalRoot, "demo")
			if err := os.MkdirAll(filepath.Join(dir, "views"), 0700); err != nil {
				t.Fatal(err)
			}
			query := view.Query{ParamsSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}
			files := map[string]any{"connector.json": map[string]any{"id": "demo", "name": "Demo", "version": "1.0.0", "type": "mcp", "authMode": "oneid"}}
			cli := mode != "stdio"
			if cli {
				files["connector.json"].(map[string]any)["type"] = "cli"
				env := map[string]any{"ONEID_EXECUTION_HELPER": "cli"}
				if mode == "cli-template" {
					env["SERVICE_TOKEN"] = "${AP_ID_TOKEN}"
					env["ONEID_REQUIRE_MAPPING"] = "1"
				}
				files["cli.json"] = map[string]any{"env": env, "versionCheck": map[string]any{"command": map[string]any{"darwin": "helper --version", "linux": "helper --version", "win32": "helper.exe --version"}, "minVersion": "1.0.0"}}
				query.Adapter = "cli"
				query.Args = []view.QueryArg{{Literal: "-test.run=^TestOneIDExecutionHelper$"}}
			} else {
				files["mcp.json"] = map[string]any{"mcpServers": map[string]any{"main": map[string]any{"type": "stdio", "command": os.Args[0], "args": []string{"-test.run=^TestOneIDExecutionHelper$"}, "env": map[string]any{"ONEID_EXECUTION_HELPER": "stdio", "SERVICE_TOKEN": "${ONEID_TOKEN}", "ONEID_REQUIRE_MAPPING": "1"}}}}
				query.Adapter = "mcp"
				query.Component = "main"
				query.ToolName = "read"
			}
			action := view.Action{Query: query}
			def := view.Definition{Description: "Identity test", Renderer: "html", Usage: []string{"display"}, InputSchema: json.RawMessage(`{"type":"object"}`), Entry: "views/test.html", Queries: map[string]view.Query{"read": query}, Actions: map[string]view.Action{"write": action}}
			files["view.json"] = view.Config{Views: map[string]view.Definition{"test": def}}
			for name, value := range files {
				raw, _ := json.Marshal(value)
				if err := os.WriteFile(filepath.Join(dir, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(dir, "views/test.html"), []byte("<p>test</p>"), 0600); err != nil {
				t.Fatal(err)
			}
			pkg, err := sources.Load("demo")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pkg.SetConfigured(true); err != nil {
				t.Fatal(err)
			}
			if cli {
				entry := filepath.Join(dir, "helper")
				if runtime.GOOS == "windows" {
					entry += ".exe"
				}
				data, err := os.ReadFile(os.Args[0])
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(entry, data, 0700); err != nil {
					t.Fatal(err)
				}
				digest, err := connector.CLIFileHash(entry)
				if err != nil {
					t.Fatal(err)
				}
				if err := connector.WriteInstallation(pkg.InstallRoot, connector.Installation{ConnectorID: pkg.ID, Executable: entry, ExecutableSHA256: digest}); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(t.TempDir(), "identity")
			manager := connectorauth.New(t.Context(), sources, nil).WithSsoIdTokenFile(file)
			service := &connectorops.Service{Auth: manager, Sources: sources}
			apps, entry, _ := webapptest.Open(t, `{"schemaVersion":2,"id":"webapp-0123456789abcdef","key":"oneid-app","label":"OneID","version":"1.0.0","target":"any","hosting":"platform","minPlatformVersion":"0.5.0","frontend":{"root":"frontend","index":"index.html"}}`, map[string]string{"frontend/index.html": "<h1>OneID</h1>"})
			bridge := webappbridge.New(webappbridge.Options{Apps: apps, Owner: "owner", Connectors: service})
			defer bridge.Close()

			scope := connectorops.Scope{Subject: "owner", IdempotencyNamespace: "webapp", Execution: []connectorops.Permission{{ConnectorID: "demo", Adapter: query.Adapter}}, Check: func() error { return nil }}
			req := connectorops.Request{ConnectorID: "demo", Adapter: query.Adapter, Component: query.Component, ToolName: query.ToolName}
			if cli {
				req.Args = []string{"-test.run=^TestOneIDExecutionHelper$"}
			} else {
				req.Arguments = map[string]any{}
			}
			doc := view.Document{View: view.Reference{Source: "connector", ConnectorID: "demo", Key: "test", Version: "1.0.0"}, Queries: def.Queries, Actions: def.Actions}
			reader := &viewquery.Service{Sources: sources, Connectors: service}
			writer := &viewaction.Service{Sources: sources, Connectors: service, Authorize: func(context.Context, viewaction.Context, string) error { return nil }}
			actor := viewaction.Context{ChatID: "chat", RunID: "run", AgentKey: "agent", Subject: "owner"}
			for _, generation := range []string{"a", "b"} {
				if err := os.WriteFile(file, []byte("identity-"+generation), 0600); err != nil {
					t.Fatal(err)
				}
				result, err := service.Invoke(t.Context(), scope, req)
				if err != nil {
					t.Fatal(err)
				}
				raw := result.Stdout
				if !cli {
					raw = string(result.MCP)
				}
				checkIdentityResult(t, raw, generation)
				appScope, err := apps.Acquire(entry.ID)
				if err != nil {
					t.Fatal(err)
				}
				appScope.Policy.Connectors = []webapp.ConnectorScope{{ConnectorID: "demo", Adapter: query.Adapter}}
				appReq := req
				appReq.IdempotencyKey = "webapp-" + generation
				argsJSON, _ := json.Marshal(appReq)
				var appArgs map[string]any
				json.Unmarshal(argsJSON, &appArgs)
				if !cli {
					appArgs["arguments"] = map[string]any{}
				}
				body, _ := json.Marshal(map[string]any{"action": "connector.invoke", "args": appArgs})
				rec := httptest.NewRecorder()
				request := httptest.NewRequest("POST", "http://app/__desktop/actions/call", bytes.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				bridge.ServeHTTP(rec, request, appScope)
				appScope.Release()
				if rec.Code != 200 {
					t.Fatalf("WebApp bridge: %d %s", rec.Code, rec.Body.String())
				}
				var appResult struct {
					Result connectorops.Result `json:"result"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &appResult); err != nil {
					t.Fatal(err)
				}
				appRaw := appResult.Result.Stdout
				if !cli {
					appRaw = string(appResult.Result.MCP)
				}
				checkIdentityResult(t, appRaw, generation)

				got, err := reader.Execute(t.Context(), doc, "read", json.RawMessage(`{}`), viewquery.Context{ChatID: "chat", RunID: "run"})
				if err != nil {
					t.Fatal(err)
				}
				checkIdentityResult(t, string(got.Data), generation)
				requestID := "request-" + generation
				written, err := writer.Execute(t.Context(), doc, "write", json.RawMessage(`{}`), requestID, actor)
				if err != nil {
					t.Fatal(err)
				}
				checkIdentityResult(t, string(written.Data), generation)
				replay, err := writer.Execute(t.Context(), doc, "write", json.RawMessage(`{}`), requestID, actor)
				if err != nil || replay.InvocationID != written.InvocationID {
					t.Fatalf("receipt replay failed: %v", err)
				}
			}
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
			req.IdempotencyKey = "after-logout"
			_, err = service.Invoke(t.Context(), scope, req)
			requireAuthError(t, err)
			_, err = reader.Execute(t.Context(), doc, "read", json.RawMessage(`{}`), viewquery.Context{ChatID: "chat", RunID: "run"})
			requireAuthError(t, err)
			_, err = writer.Execute(t.Context(), doc, "write", json.RawMessage(`{}`), "logout-write", actor)
			requireAuthError(t, err)
			scope.Execution = nil
			if _, err := service.Invoke(t.Context(), scope, req); err == nil || err.Error() != "connector_execution_not_allowed" {
				t.Fatalf("missing grant allowed: %v", err)
			}
			for _, name := range []string{"credentials.json", "oauth.json"} {
				if _, err := os.Stat(filepath.Join(sources.PersistentRoot(), "demo", name)); !os.IsNotExist(err) {
					t.Fatal("identity was persisted as connector credentials")
				}
			}
		})
	}
}
func checkIdentityResult(t *testing.T, raw, generation string) {
	t.Helper()
	if !strings.Contains(raw, fmt.Sprintf(`"generation":"%s"`, generation)) || !strings.Contains(raw, `"matches":true`) || strings.Contains(raw, "identity-") {
		t.Fatalf("incorrect identity result: %s", raw)
	}
}
func requireAuthError(t *testing.T, err error) {
	t.Helper()
	var failure *connectorops.Error
	if !errors.As(err, &failure) || failure.Code != "connector_auth_required" {
		t.Fatalf("logout error: %v", err)
	}
}
