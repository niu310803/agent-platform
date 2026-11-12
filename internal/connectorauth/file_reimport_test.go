package connectorauth

import (
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/connector"
)

func TestFileReimportRejectsOldAuthorizationAndAllowsNewConnection(t *testing.T) {
	for _, mode := range []string{"self", "oneid", "mcp"} {
		t.Run(mode, func(t *testing.T) {
			sources := deletionFixture(t, mode)
			pkg, err := sources.Load("demo")
			if err != nil {
				t.Fatal(err)
			}
			before, err := readAuthState(sources.PersistentRoot(), "demo")
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{}
			for _, name := range []string{"connector.json", "mcp.json"} {
				files[name], err = os.ReadFile(filepath.Join(pkg.Dir, name))
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := connector.DeletePackage(t.Context(), sources, "demo", nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(pkg.Dir, 0755); err != nil {
				t.Fatal(err)
			}
			for name, data := range files {
				if err := os.WriteFile(filepath.Join(pkg.Dir, name), data, 0644); err != nil {
					t.Fatal(err)
				}
			}
			// Ordinary reads must neither reactivate nor complete configuration.
			m := New(t.Context(), sources, nil)
			if _, err := m.Connection(t.Context(), "demo"); err != nil {
				t.Fatal(err)
			}
			control, _ := connector.ReadStateControl(sources.PersistentRoot(), "demo")
			if !control.Deleted {
				t.Fatal("status read reactivated the installation")
			}
			if err := sources.ReconcileInstallations(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := m.markConfiguredAt(pkg, m.epoch("demo"), before.Generation); err == nil {
				t.Fatal("old confirmation configured the new installation")
			}
			if err := m.completeLogin(t.Context(), pkg, m.epoch("demo"), before.Generation); err == nil {
				t.Fatal("old login configured the new installation")
			}
			if mode == "mcp" {
				path := filepath.Join(sources.PersistentRoot(), "demo", "discovery-old.json")
				if err := saveLoginDiscovery(t.Context(), pkg, before.Generation, path, map[string]any{"old": true}); err == nil {
					t.Fatal("old discovery wrote credentials")
				}
				if err := saveRegisteredClient(t.Context(), pkg, OAuthClientInfo{ClientID: "old-client"}, before.Generation); err == nil {
					t.Fatal("old registration wrote credentials")
				}
			}
			requireNoPrivateState(t, sources)
			if mode == "mcp" {
				return
			}
			if mode == "oneid" {
				identity := filepath.Join(t.TempDir(), "identity")
				if err := os.WriteFile(identity, []byte("current-identity"), 0600); err != nil {
					t.Fatal(err)
				}
				m.WithSsoIdTokenFile(identity)
			}
			if _, err := m.Connect("demo"); err != nil {
				t.Fatalf("fresh confirmation failed: %v", err)
			}
			state, err := m.Connection(t.Context(), "demo")
			if err != nil || !state.Configured || state.Readiness != "ready" {
				t.Fatalf("not ready: %+v %v", state, err)
			}
		})
	}
}
