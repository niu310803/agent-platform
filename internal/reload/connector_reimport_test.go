package reload

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-platform/internal/catalog"
	"agent-platform/internal/config"
	"agent-platform/internal/connector"
	"agent-platform/internal/connectorauth"
)

func TestConnectorWatcherReactivatesFileReimport(t *testing.T) {
	root := t.TempDir()
	cfg := config.Config{Paths: config.PathsConfig{
		AgentsDir: filepath.Join(root, "agents"), RUAgentsDir: filepath.Join(root, "ru-agents"),
		SkillsCenterDir: filepath.Join(root, "skills-center"), ConnectorsCenterDir: filepath.Join(root, "connectors-center"),
	}}
	sources := cfg.Paths.ConnectorSources()
	dir := filepath.Join(sources.ExternalRoot, "demo")
	write := func() {
		t.Helper()
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		writeWatchTestFile(t, filepath.Join(dir, "connector.json"), `{"id":"demo","name":"Demo","version":"1.0.0","type":"mcp","authMode":"self"}`)
		writeWatchTestFile(t, filepath.Join(dir, "mcp.json"), `{"mcpServers":{"main":{"type":"streamableHttp","url":"https://example.test/mcp"}}}`)
	}
	write()
	if err := connector.DeletePackage(t.Context(), sources, "demo", nil, nil); err != nil {
		t.Fatal(err)
	}
	registry, err := catalog.NewFileRegistry(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := NewRuntimeCatalogReloader(registry, nil, nil, nil, "", nil)
	reasons := make(chan string, 32)
	r.AddObserver(watchTestObserver{reasons: reasons})
	ctx, cancel := context.WithCancel(t.Context())
	done := StartBackgroundReloaders(ctx, cfg, r)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("watchers did not stop")
		}
	})
	write()
	assertReloadReason(t, reasons, "connectors", 5*time.Second)
	auth := connectorauth.New(t.Context(), sources, nil)
	before, err := auth.Connection(t.Context(), "demo")
	if err != nil || before.Configured {
		t.Fatalf("unexpected initial configuration: %+v %v", before, err)
	}
	if _, err := auth.Connect("demo"); err != nil {
		t.Fatalf("file import could not connect: %v", err)
	}
	after, err := auth.Connection(t.Context(), "demo")
	if err != nil || !after.Configured || after.Readiness != "ready" {
		t.Fatalf("not ready: %+v %v", after, err)
	}
}
