package accesspolicy

import (
	"agent-platform/internal/config"
	"agent-platform/internal/contracts"
	"os"
	"path/filepath"
	"testing"
)

func TestAuthorizedChatFilesAreHostReadOnlyAndCanonical(t *testing.T) {
	root := t.TempDir()
	chats := filepath.Join(root, "chats")
	for _, dir := range []string{"current", "peer", "foreign"} {
		if err := os.MkdirAll(filepath.Join(chats, dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(chats, "foreign"), filepath.Join(chats, "peer", "escape")); err != nil {
		t.Fatal(err)
	}
	session := contracts.QuerySession{ChatRoot: filepath.Join(chats, "current"), AccessLevel: contracts.AccessLevelAskForApproval,
		RuntimeContext:   contracts.RuntimeRequestContext{LocalPaths: contracts.LocalPaths{ChatsDir: chats}},
		CanReadChatFiles: func(id string) bool { return id == "peer" },
	}
	for _, tc := range []struct {
		path  string
		mode  AccessMode
		allow bool
	}{
		{"peer/report.pdf", ReadAccess, true}, {"peer/artifacts/run/report.pdf", ReadAccess, true},
		{"peer/report.pdf", WriteAccess, false}, {"foreign/report.pdf", ReadAccess, false},
		{"peer.jsonl", ReadAccess, false}, {"mail.db", ReadAccess, false}, {"peer/.btw/run/file", ReadAccess, false},
		{"peer/.explain/run/file", ReadAccess, false}, {"peer/escape/secret", ReadAccess, false},
	} {
		plan, err := BuildPathPlan(config.AccessPolicyConfig{}, session, tc.mode, filepath.Join(chats, tc.path))
		if err != nil || plan.Allowed() != tc.allow {
			t.Fatalf("%s %s: %+v %v", tc.mode, tc.path, plan, err)
		}
	}
	recursive := BuildSubtreePlan(config.AccessPolicyConfig{}, session, ReadAccess, filepath.Join(chats, "peer"))
	if !recursive.Blocked() {
		t.Fatalf("recursive peer read included side lanes: %+v", recursive)
	}
	session.AgentHasRuntimeSandbox = true
	plan, err := BuildPathPlan(config.AccessPolicyConfig{}, session, ReadAccess, filepath.Join(chats, "peer/report.pdf"))
	if err != nil || plan.Allowed() {
		t.Fatalf("Host exception escaped to container: %+v %v", plan, err)
	}
}
