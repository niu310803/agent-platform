// Package webapptest provides preinstalled single-instance fixtures.
package webapptest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/config"
	"agent-platform/internal/webapp"
)

func Open(t testing.TB, manifest string, files map[string]string) (*webapp.Service, webapp.Entry, webapp.Options) {
	t.Helper()
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "webapp.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	o := webapp.Options{Root: filepath.Join(root, ".state", "webapps"), DataRoot: filepath.Join(root, "webapp-data"), SingleRoot: root, Config: config.DefaultWebAppConfig(), PlatformVersion: "999.0.0"}
	s, err := webapp.Open(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := s.RestoreDesired(context.Background()); err != nil {
		t.Fatal(err)
	}
	return s, s.Current(), o
}
