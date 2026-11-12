package catalog

import (
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/config"
	"agent-platform/internal/connector"
)

func TestCatalogReactivatesFileReimportAtStartupAndReload(t *testing.T) {
	for _, startup := range []bool{true, false} {
		t.Run(map[bool]string{true: "startup", false: "reload"}[startup], func(t *testing.T) {
			root := t.TempDir()
			cfg := config.Config{Paths: config.PathsConfig{
				AgentsDir: filepath.Join(root, "agents"), RUAgentsDir: filepath.Join(root, "ru-agents"),
				SkillsCenterDir: filepath.Join(root, "skills-center"), ConnectorsCenterDir: filepath.Join(root, "connectors-center"),
				StateDir: filepath.Join(t.TempDir(), "custom-state"),
			}}
			sources := cfg.Paths.ConnectorSources()
			write := func() {
				writeRuntimeAssemblerFile(t, filepath.Join(sources.ExternalRoot, "demo", "connector.json"), `{"id":"demo","name":"Demo","version":"1.0.0","type":"mcp","authMode":"oneid"}`)
				writeRuntimeAssemblerFile(t, filepath.Join(sources.ExternalRoot, "demo", "mcp.json"), `{"mcpServers":{"main":{"type":"streamableHttp","url":"https://example.test/mcp"}}}`)
			}
			write()
			if err := connector.DeletePackage(t.Context(), sources, "demo", nil, nil); err != nil {
				t.Fatal(err)
			}
			deleted, _ := connector.ReadStateControl(sources.PersistentRoot(), "demo")
			var registry *FileRegistry
			var err error
			if !startup {
				registry, err = newVersionTestRegistry(t, cfg, nil)
				if err != nil {
					t.Fatal(err)
				}
			}
			write()
			if startup {
				_, err = newVersionTestRegistry(t, cfg, nil)
			} else {
				err = registry.Reload(t.Context(), "agents") // connectors watcher cascades here
			}
			if err != nil {
				t.Fatal(err)
			}
			control, err := connector.ReadStateControl(sources.PersistentRoot(), "demo")
			if err != nil || control.Deleted || control.Generation != deleted.Generation {
				t.Fatalf("not reactivated: %+v %v", control, err)
			}
			if _, err := os.Stat(filepath.Join(sources.PersistentRoot(), "demo")); !os.IsNotExist(err) {
				t.Fatalf("configuration/credentials recreated: %v", err)
			}
		})
	}
}
