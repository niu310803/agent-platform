package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	platform "agent-platform"
	"agent-platform/internal/config"
	"agent-platform/internal/runtimeskills"
)

func TestSingleAssemblyOwnerReadOnlyAndPrunedModules(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	t.Cleanup(func() { _ = runtimeskills.Remove(root) })
	for _, key := range []string{"AP_RUNTIME_CHATS_DIR", "AP_RUNTIME_MEMORY_DIR", "AP_RUNTIME_PAN_DIR", "AP_RUNTIME_STATE_DIR"} {
		t.Setenv(key, "")
	}
	t.Setenv("AP_RUNTIME_DIR", root)
	registries := t.TempDir()
	t.Setenv("AP_RUNTIME_REGISTRIES_DIR", registries)
	manifest := map[string]any{"schemaVersion": 2, "id": "webapp-0123456789abcdef", "key": "single", "label": "Single", "version": "1.0.0", "target": "any", "hosting": "platform", "minPlatformVersion": platform.Version(), "frontend": map[string]any{"root": "frontend", "index": "index.html"}}
	raw, _ := json.Marshal(manifest)
	for name, value := range map[string]string{"webapp.json": string(raw), "frontend/index.html": "<html>single</html>", "configs/tools.yml": "bash:\n  git-bash:\n    enabled: false\n"} {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, err := New(context.Background(), config.LoadOptions{ConfigDir: root, WebAppHost: "single", Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.mailbox != nil || a.memoryWorker != nil || a.voice != nil || a.lspManager != nil || a.kbasesService != nil || a.automation != nil || a.gateways != nil {
		t.Fatalf("single started pruned modules: mailbox=%t memory=%t voice=%t lsp=%t kbases=%t automation=%t gateways=%t", a.mailbox != nil, a.memoryWorker != nil, a.voice != nil, a.lspManager != nil, a.kbasesService != nil, a.automation != nil, a.gateways != nil)
	}
	origin := "http://127.0.0.1:12345"
	if _, err := a.Router.PrepareSingleOrigin(origin); err != nil {
		t.Fatal(err)
	}
	var credential struct {
		Token string `json:"token"`
	}
	b, err := os.ReadFile(filepath.Join(root, ".state/webapp-owner/owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &credential); err != nil || credential.Token == "" {
		t.Fatal("missing owner")
	}
	if second, err := New(context.Background(), config.LoadOptions{ConfigDir: root, WebAppHost: "single"}); err == nil {
		second.Close()
		t.Fatal("second Platform initialized the same runtime")
	}
	if after, err := os.ReadFile(filepath.Join(root, ".state/webapp-owner/owner.json")); err != nil || string(after) != string(b) {
		t.Fatal("rejected second instance rotated active owner credential")
	}
	for _, path := range []string{"/api/admin/agents", "/api/webapp/status"} {
		req := httptest.NewRequest("GET", origin+path, nil)
		req.Header.Set("Authorization", "Bearer "+credential.Token)
		w := httptest.NewRecorder()
		a.Router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{"/api/admin/agent", "/api/terminal/open", "/api/automations", "/api/memory", "/api/kbases", "/api/send"} {
		req := httptest.NewRequest("POST", origin+path, nil)
		req.Header.Set("Authorization", "Bearer "+credential.Token)
		w := httptest.NewRecorder()
		a.Router.ServeHTTP(w, req)
		if w.Code != 404 {
			t.Fatalf("enabled %s: %d", path, w.Code)
		}
	}
	for _, name := range []string{"automations", "kbases", "ru-kbases", "memory", "chats/mail.db"} {
		if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Fatalf("pruned path created: %s (%v)", name, err)
		}
	}
	entries, err := os.ReadDir(registries)
	if err != nil || len(entries) != 0 {
		t.Fatalf("shared registries changed: %v %v", entries, err)
	}
	b, _ = os.ReadFile(filepath.Join(root, "webapp.json"))
	if string(b) != string(raw) {
		t.Fatal("package changed")
	}
}
