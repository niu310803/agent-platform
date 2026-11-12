package connectorops

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"agent-platform/internal/connector"
	"agent-platform/internal/connectorauth"
)

type oneIDExecutionFixture struct {
	name, cli, mcp string
}

func oneIDExecutionFixtures() []oneIDExecutionFixture {
	const httpMCP = `{"mcpServers":{"main":{"type":"streamableHttp","url":"https://example.test/mcp"}}}`
	return []oneIDExecutionFixture{
		{name: "CLI without templates", cli: `{}`},
		{name: "CLI with templates", cli: `{"env":{"SERVICE_TOKEN":"${AP_ID_TOKEN}"}}`},
		{name: "HTTP MCP", mcp: httpMCP},
		{name: "stdio MCP", mcp: `{"mcpServers":{"main":{"type":"stdio","command":"fixture-mcp"}}}`},
		{name: "CLI and MCP", cli: `{}`, mcp: httpMCP},
	}
}

func writeOneIDExecutionFixture(t *testing.T, fixture oneIDExecutionFixture, mode connector.AuthMode) (connector.Sources, connector.Package) {
	t.Helper()
	sources := connector.Sources{ExternalRoot: t.TempDir(), StateRoot: t.TempDir()}
	dir := filepath.Join(sources.ExternalRoot, "demo")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	kind := "mcp"
	if fixture.cli != "" {
		kind = "cli"
	}
	files := map[string]string{"connector.json": `{"id":"demo","name":"Demo","version":"1.0.0","type":"` + kind + `","authMode":"` + string(mode) + `"}`}
	if fixture.cli != "" {
		files["cli.json"] = fixture.cli
	}
	if fixture.mcp != "" {
		files["mcp.json"] = fixture.mcp
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	pkg, err := sources.Load("demo")
	if err != nil {
		t.Fatal(err)
	}
	return sources, pkg
}

func TestOneIDRequiresIdentityBeforeWriteReceipt(t *testing.T) {
	for _, fixture := range oneIDExecutionFixtures() {
		for _, identityState := range []string{"missing", "empty", "invalid"} {
			name := fixture.name + "/" + identityState
			t.Run(name, func(t *testing.T) {
				sources, pkg := writeOneIDExecutionFixture(t, fixture, connector.AuthOneID)
				if _, err := pkg.SetConfigured(true); err != nil {
					t.Fatal(err)
				}
				identity := filepath.Join(t.TempDir(), "identity")
				wantStatus := "unauthorized"
				if identityState != "missing" {
					value := ""
					if identityState == "invalid" {
						value = "invalid\ntoken"
					}
					if err := os.WriteFile(identity, []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
				}
				manager := connectorauth.New(t.Context(), sources, nil).WithSsoIdTokenFile(identity)
				status, err := manager.StatusComponent(t.Context(), "demo", "")
				if err != nil || status.Status != wantStatus {
					t.Fatalf("identity status = %q, err = %v", status.Status, err)
				}
				service := Service{Auth: manager, Sources: sources}
				scope := Scope{Subject: "owner", IdempotencyNamespace: "fixture", Execution: []Permission{{ConnectorID: "demo", Adapter: "cli"}, {ConnectorID: "demo", Adapter: "mcp"}}, Check: func() error { return nil }}
				var requests []Request
				if fixture.cli != "" {
					requests = append(requests, Request{ConnectorID: "demo", Adapter: "cli", Args: []string{"business"}})
				}
				if fixture.mcp != "" {
					requests = append(requests, Request{ConnectorID: "demo", Adapter: "mcp", Component: "main", ToolName: "business", Arguments: map[string]any{}})
				}
				for _, req := range requests {
					for _, key := range []string{"", "oneid-write-123"} {
						req.IdempotencyKey = key
						for range 2 {
							_, err := service.Invoke(t.Context(), scope, req)
							var failure *Error
							if !errors.As(err, &failure) || failure.Code != "connector_auth_required" || failure.Status != http.StatusUnauthorized {
								t.Fatalf("adapter %q key %q: unauthenticated oneid returned %v", req.Adapter, key, err)
							}
						}
					}
				}
				if _, err := os.Stat(filepath.Join(sources.PersistentRoot(), "demo", "invocations")); !os.IsNotExist(err) {
					t.Fatalf("unauthenticated oneid created write receipts: %v", err)
				}
				scope.Execution = nil
				_, err = service.Invoke(t.Context(), scope, requests[0])
				if err == nil || err.Error() != "connector_execution_not_allowed" {
					t.Fatalf("unauthenticated oneid bypassed permission check: %v", err)
				}
			})
		}
	}
}

func TestOneIDCatalogAdvertisesExecutionCapabilities(t *testing.T) {
	for _, fixture := range oneIDExecutionFixtures() {
		for _, mode := range []connector.AuthMode{connector.AuthOneID, connector.AuthDelegated} {
			// The credential template belongs to OneID only.
			if mode != connector.AuthOneID && fixture.name == "CLI with templates" {
				continue
			}
			t.Run(fixture.name+"/"+string(mode), func(t *testing.T) {
				sources, pkg := writeOneIDExecutionFixture(t, fixture, mode)
				wantAdapters, wantComponents := []string{}, []string{}
				if fixture.cli != "" {
					wantAdapters = append(wantAdapters, "cli")
				}
				if fixture.mcp != "" {
					wantAdapters = append(wantAdapters, "mcp")
					wantComponents = append(wantComponents, "main")
				}
				check := func(c Catalog, err error) {
					t.Helper()
					if err != nil || c.ConnectorID != pkg.ID || c.Revision == "" || !reflect.DeepEqual(c.Adapters, wantAdapters) || !reflect.DeepEqual(c.Components, wantComponents) {
						t.Fatalf("catalog = %#v, err = %v", c, err)
					}
				}
				check(Load(pkg))
				service := Service{Sources: sources}
				scope := Scope{Subject: "owner", Execution: []Permission{{ConnectorID: "demo", Adapter: "cli"}, {ConnectorID: "demo", Adapter: "mcp"}}, Check: func() error { return nil }}
				check(service.Describe(scope, "demo"))
			})
		}
	}
}
