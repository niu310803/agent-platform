package webapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/config"
)

func singleFixture(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	if err := os.Mkdir(filepath.Join(root, "frontend"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "frontend/index.html"), []byte("single"), 0600); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]any{"schemaVersion": 2, "id": "webapp-0123456789abcdef", "key": "single", "label": "Single", "version": "1.0.0", "target": "any", "hosting": "platform", "minPlatformVersion": "0.5.0", "frontend": map[string]any{"root": "frontend", "index": "index.html"}}
	raw, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(root, "webapp.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return Options{SingleRoot: root, Root: filepath.Join(root, ".state/webapps"), DataRoot: filepath.Join(root, "webapp-data"), Config: config.DefaultWebAppsConfig(), PlatformVersion: "0.5.46"}
}

func TestSingleFlatRootsLockVersionAndIdentity(t *testing.T) {
	o := singleFixture(t)
	s, err := Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(o); err == nil {
		t.Fatal("second instance acquired host lock")
	}
	if err = s.RestoreDesired(context.Background()); err != nil {
		t.Fatal(err)
	}
	scope, err := s.Acquire("webapp-0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if scope.CodeRoot != o.SingleRoot || scope.DataRoot != o.DataRoot {
		t.Fatalf("wrong roots: %s %s", scope.CodeRoot, scope.DataRoot)
	}
	identity := scope.InstallationID
	scope.Release()
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(o)
	if err != nil {
		t.Fatal(err)
	}
	if s.records["webapp-0123456789abcdef"].InstallationID != identity {
		t.Fatal("restart changed installation identity")
	}
	s.Close()
	o.PlatformVersion = "0.4.0"
	if _, err := Open(o); err == nil {
		t.Fatal("ignored minimum version")
	}
}

func TestSingleRejectsPackageSymlink(t *testing.T) {
	o := singleFixture(t)
	if err := os.Symlink(o.Root, filepath.Join(o.SingleRoot, "frontend", "escape")); err != nil {
		t.Skip(err)
	}
	if _, err := Open(o); err == nil {
		t.Fatal("accepted package symlink")
	}
}
