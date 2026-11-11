package webapphost

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
)

func InspectSingle(ctx context.Context, node string) (Status, error) {
	if runtime.GOOS == "linux" {
		if node != "" && node != NodePath {
			return Status{}, fmt.Errorf("Linux single uses the image-managed Node at %s", NodePath)
		}
		s := Inspect(ctx)
		s.Host = "single"
		return s, nil
	}
	s := Status{Host: "single", OS: runtime.GOOS, Arch: runtime.GOARCH, Stage: "foundation", Issues: []Issue{}, Warnings: []Issue{}}
	if node == "" {
		return s, nil
	} // Static-only packages need no Node runtime.
	if !filepath.IsAbs(node) {
		return s, fmt.Errorf("webapp-node must be absolute")
	}
	raw, err := runProbe(ctx, node, "--version")
	version := strings.TrimSpace(string(raw))
	if err != nil || !strings.HasPrefix(version, "v24.") {
		return s, fmt.Errorf("webapp-node must be Node 24 (got %q): %v", version, err)
	}
	s.NodeVersion = version
	s.NodeReady = true
	s.IsolationReady = true
	s.IsolationProfile = "node-permission"
	s.BackendPrerequisitesReady = true
	s.Warnings = append(s.Warnings, Issue{Code: "webapp_network_isolation_unavailable"})
	return s, nil
}
