package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/webapp"
)

func TestWebAppCommandIsOfflineAndUsesCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("AP_RUNTIME_DIR", filepath.Join(root, "must-not-create-runtime"))
	if err := os.Mkdir("frontend", 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("frontend/index.html", []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"schemaVersion":2,"id":"webapp-0123456789abcdef","key":"cli-test","label":"CLI test","version":"1.0.0","hosting":"platform","target":"any","minPlatformVersion":"0.5.46","frontend":{"root":"frontend","index":"index.html"}}`)
	if err := os.WriteFile("webapp.json", raw, 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"check"}, {"pack"}, {"check", "--source", "dist/cli-test-1.0.0.zip"}} {
		var out bytes.Buffer
		if err := runWebAppCommand(args, &out); err != nil {
			t.Fatal(err)
		}
		var report webapp.PackageReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.AppID != "webapp-0123456789abcdef" || report.FileCount != 2 {
			t.Fatalf("bad CLI report: %s %v", &out, err)
		}
	}
	if _, err := os.Stat("must-not-create-runtime"); !os.IsNotExist(err) {
		t.Fatalf("offline CLI touched runtime: %v", err)
	}
}

func TestWebAppCommandArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"install"}, {"pack", "--unknown"}, {"check", "--output", "a.zip"}, {"check", "--source", "a", "b"}, {"check", "a", "b"}} {
		if err := runWebAppCommand(args, &bytes.Buffer{}); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	if err := runWebAppCommand([]string{"pack", "--help"}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}
