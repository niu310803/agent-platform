package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestSingleStartupOptionsAndReady(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "ready.json")
	o, err := parseConfigOptions([]string{"--webapp-host=single", "--listen=127.0.0.1:0", "--ready-file=" + path, "--parent-pid=42"})
	if err != nil || o.Listen != "127.0.0.1:0" || o.ParentPID != 42 {
		t.Fatal(o, err)
	}
	if err = writeReady(path, "http://127.0.0.1:1234", "http://127.0.0.1:1235"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result["pid"] != float64(os.Getpid()) || result["platformVersion"] == "" {
		t.Fatal(string(raw))
	}
	for _, args := range [][]string{{"--listen=invalid"}, {"--listen=127.0.0.1:0", "--port=80"}, {"--parent-pid=-2"}, {"--ready-file=relative"}} {
		if _, err := parseConfigOptions(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if got := listenerURL(&net.TCPAddr{IP: net.IPv4zero, Port: 1234}); got != "http://127.0.0.1:1234" {
		t.Fatal(got)
	}
}
