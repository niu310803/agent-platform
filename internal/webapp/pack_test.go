package webapp

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func toolingManifest(backend bool) []byte {
	m := map[string]any{"schemaVersion": 2, "id": "webapp-0123456789abcdef", "key": "tooling-test", "label": "Tooling test", "version": "1.0.0", "hosting": "platform", "target": "any", "minPlatformVersion": "0.5.46", "frontend": map[string]any{"root": "frontend", "index": "index.html"}}
	if backend {
		m["backend"] = map[string]any{"command": map[string]string{"type": "node", "entry": "backend/server.mjs"}, "health": map[string]string{"type": "http", "path": "/health"}}
	}
	raw, _ := json.Marshal(m)
	return raw
}

func toolingSource(t *testing.T, backend bool) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{"webapp.json": string(toolingManifest(backend)), "AGENTS.md": "app instructions", "frontend/index.html": "<main>hello</main>", "frontend/中文.txt": "审阅报告", "backend/server.mjs": "// Node source", "platform/agents/auditor/agent.yml": "key: auditor", "platform/registries/models/test.yml": "key: test", "platform/skills-center/sample/SKILL.md": "# Sample", "platform/connectors-center/sample/bin/tool": "#!/bin/sh\necho tool\n", "platform/configs/tools.yml": "public: true", "platform/registries/private.local.yml": "private config", "platform/registries/private.pem": "private key", "platform/registries/.env": "secret", "frontend/node_modules/private": "dependency cache", ".runtime/.state/owner.json": "runtime identity", "scripts/pack.mjs": "unselected development tool"}
	for name, text := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "platform/connectors-center/sample/bin/tool"), 0755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestPackageToolingFiveRootsRoundTripAndNoManifestRewrite(t *testing.T) {
	for _, backend := range []bool{false, true} {
		t.Run(map[bool]string{false: "frontend", true: "Node-backend"}[backend], func(t *testing.T) {
			source := toolingSource(t, backend)
			raw, _ := os.ReadFile(filepath.Join(source, "webapp.json"))
			checked, err := CheckPackage(context.Background(), source)
			if err != nil {
				t.Fatal(err)
			}
			packed, err := PackPackage(context.Background(), source, "")
			if err != nil {
				t.Fatal(err)
			}
			if packed.Output != filepath.Join(checked.Source, "dist/tooling-test-1.0.0.zip") || len(packed.SHA256) != 64 || packed.FileCount != checked.FileCount || packed.ExpandedBytes != checked.ExpandedBytes {
				t.Fatalf("bad package report: %+v", packed)
			}
			z, err := zip.OpenReader(packed.Output)
			if err != nil {
				t.Fatal(err)
			}
			defer z.Close()
			roots := map[string]bool{}
			for _, file := range z.File {
				roots[strings.Split(strings.TrimSuffix(file.Name, "/"), "/")[0]] = true
				if strings.Contains(file.Name, "private") || strings.Contains(file.Name, "node_modules") || strings.HasPrefix(file.Name, "scripts/") {
					t.Fatalf("unwanted file: %s", file.Name)
				}
				if file.Name == "webapp.json" {
					reader, _ := file.Open()
					got, _ := io.ReadAll(reader)
					reader.Close()
					if !bytes.Equal(got, raw) {
						t.Fatal("manifest was rewritten")
					}
				}
				if runtime.GOOS != "windows" && strings.HasSuffix(file.Name, "/bin/tool") && file.Mode()&0111 == 0 {
					t.Fatal("connector executable permission was lost")
				}
			}
			want := map[string]bool{"backend": true, "frontend": true, "platform": true, "webapp.json": true, "AGENTS.md": true}
			if !reflect.DeepEqual(roots, want) {
				t.Fatalf("unexpected package roots: %v", roots)
			}
			again, err := CheckPackage(context.Background(), packed.Output)
			if err != nil || again.FileCount != checked.FileCount || again.ExpandedBytes != checked.ExpandedBytes {
				t.Fatalf("ZIP check: %+v %v", again, err)
			}
			before, _ := os.ReadFile(packed.Output)
			if _, err := PackPackage(context.Background(), source, packed.Output); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(packed.Output)
			if !bytes.Equal(before, after) {
				t.Fatal("package is not reproducible")
			}
			state, _ := os.ReadFile(filepath.Join(source, ".runtime/.state/owner.json"))
			if string(state) != "runtime identity" {
				t.Fatal("runtime state changed")
			}
		})
	}
}

func TestPackageToolingUsesSharedManifestAndChecksEntries(t *testing.T) {
	for name, manifest := range map[string][]byte{"duplicate-key": bytes.Replace(toolingManifest(false), []byte(`"version":"1.0.0"`), []byte(`"version":"1.0.0","version":"2.0.0"`), 1), "unknown-field": bytes.Replace(toolingManifest(false), []byte(`"label":"Tooling test"`), []byte(`"label":"Tooling test","unknown":true`), 1), "backend-missing": bytes.Replace(toolingManifest(true), []byte("backend/server.mjs"), []byte("backend/missing.mjs"), 1)} {
		t.Run(name, func(t *testing.T) {
			source := toolingSource(t, false)
			if err := os.WriteFile(filepath.Join(source, "webapp.json"), manifest, 0644); err != nil {
				t.Fatal(err)
			}
			if _, err := CheckPackage(context.Background(), source); err == nil {
				t.Fatal("invalid package accepted")
			}
		})
	}
	source := toolingSource(t, false)
	if err := os.Remove(filepath.Join(source, "frontend/index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckPackage(context.Background(), source); err == nil {
		t.Fatal("missing frontend accepted")
	}
}

func toolingZIP(t *testing.T, extra []packageFile) string {
	t.Helper()
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	files := append([]packageFile{{"webapp.json", toolingManifest(false), 0644}, {"frontend/index.html", []byte("<main>test</main>"), 0644}}, extra...)
	for _, file := range files {
		header := &zip.FileHeader{Name: file.name, Method: zip.Store}
		header.SetMode(file.mode)
		f, err := w.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(file.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(t.TempDir(), "app.zip")
	if err := os.WriteFile(name, body.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestPackageToolingRejectsUnsafeZIPs(t *testing.T) {
	cases := map[string][]packageFile{
		"traversal":        {{"../escape", nil, 0644}},
		"absolute":         {{"/frontend/a", nil, 0644}},
		"wrapper":          {{"webapp-0123456789abcdef/extra", nil, 0644}},
		"legacy-root":      {{"agents/test/agent.yml", nil, 0644}},
		"private-config":   {{"platform/registries/private.local.yml", nil, 0644}},
		"unknown-platform": {{"platform/.state/owner.json", nil, 0644}},
		"duplicate":        {{"frontend/index.html", nil, 0644}},
		"case-collision":   {{"frontend/INDEX.HTML", nil, 0644}},
		"parent-case":      {{"frontend/Dir/a", nil, 0644}, {"frontend/dir/b", nil, 0644}},
		"file-directory":   {{"frontend/dir", nil, 0644}, {"frontend/dir/a", nil, 0644}},
		"symlink":          {{"frontend/link", []byte("/etc/passwd"), fs.ModeSymlink | 0777}},
		"wrong-root-type":  {{"backend", nil, 0644}},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := CheckPackage(context.Background(), toolingZIP(t, files)); err == nil {
				t.Fatal("unsafe ZIP accepted")
			}
		})
	}
	var body bytes.Buffer
	w := zip.NewWriter(&body)
	f, err := w.CreateRaw(&zip.FileHeader{Name: "frontend/oversized", Method: zip.Store, UncompressedSize64: packageByteLimit + 1})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(nil)
	_ = w.Close()
	name := filepath.Join(t.TempDir(), "oversized.zip")
	_ = os.WriteFile(name, body.Bytes(), 0644)
	if _, err := CheckPackage(context.Background(), name); err == nil {
		t.Fatal("oversized ZIP accepted")
	}
}

func TestPackageToolingRejectsSourceLinksAndPreservesOldOutputOnFailure(t *testing.T) {
	source := toolingSource(t, false)
	output := filepath.Join(t.TempDir(), "app.zip")
	if err := os.WriteFile(output, []byte("old output"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "frontend/index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := PackPackage(context.Background(), source, output); err == nil {
		t.Fatal("invalid source packed")
	}
	old, _ := os.ReadFile(output)
	if string(old) != "old output" {
		t.Fatal("previous output lost")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink(output, filepath.Join(source, "frontend/index.html")); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckPackage(context.Background(), source); err == nil {
		t.Fatal("source link accepted")
	}
}

func TestPackageToolingOutputAndCancellation(t *testing.T) {
	source := toolingSource(t, false)
	if _, err := PackPackage(context.Background(), source, filepath.Join(source, "frontend/new/app.zip")); err == nil {
		t.Fatal("output inside source accepted")
	}
	if _, err := os.Stat(filepath.Join(source, "frontend/new")); !os.IsNotExist(err) {
		t.Fatalf("rejected output changed source: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CheckPackage(ctx, source); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := PackPackage(ctx, source, ""); err != context.Canceled {
		t.Fatalf("cancel: %v", err)
	}
}

func TestPackageToolingExcludesAllEnvPrefixes(t *testing.T) {
	names := []string{".env", ".env.production", ".envrc", ".environment", ".ENVRC"}
	source := toolingSource(t, false)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(source, "frontend", name), []byte("private environment"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	report, err := PackPackage(context.Background(), source, "")
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.OpenReader(report.Output)
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	for _, file := range z.File {
		for _, name := range names {
			if file.Name == "frontend/"+name {
				t.Errorf("private environment file was packaged: %s", file.Name)
			}
		}
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			archive := toolingZIP(t, []packageFile{{"frontend/" + name, []byte("private environment"), 0644}})
			if _, err := CheckPackage(context.Background(), archive); err == nil {
				t.Fatal("ZIP with private environment file accepted")
			}
		})
	}
}

func TestPackageToolingRejectsOversizedOutputAndPreservesOldOutput(t *testing.T) {
	source := t.TempDir()
	manifest := toolingManifest(false)
	if err := os.WriteFile(filepath.Join(source, "webapp.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(source, "frontend"), 0755); err != nil {
		t.Fatal(err)
	}
	entry, err := os.Create(filepath.Join(source, "frontend/index.html"))
	if err != nil {
		t.Fatal(err)
	}
	// Valid expanded content at the limit; incompressible bytes plus ZIP
	// headers exceed the separate limit on the finished archive.
	_, writeErr := io.CopyN(entry, rand.New(rand.NewSource(1)), packageByteLimit-int64(len(manifest)))
	closeErr := entry.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("create boundary input: %v %v", writeErr, closeErr)
	}
	output := filepath.Join(t.TempDir(), "app.zip")
	if err := os.WriteFile(output, []byte("old output"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := PackPackage(context.Background(), source, output); err == nil || !strings.Contains(err.Error(), "ZIP size limit exceeded") {
		t.Fatalf("expected finished ZIP size rejection, got %v", err)
	}
	old, err := os.ReadFile(output)
	if err != nil || string(old) != "old output" {
		t.Fatalf("previous output changed: %q %v", old, err)
	}
	entries, err := os.ReadDir(filepath.Dir(output))
	if err != nil || len(entries) != 1 || entries[0].Name() != "app.zip" {
		t.Fatalf("temporary output was not cleaned up: %v %v", entries, err)
	}
}
