package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agent-platform/internal/chat"
	"agent-platform/internal/contracts"
	runtimetypes "agent-platform/internal/runtime/types"
)

func TestNewRunInheritsChatAuthoredFiles(t *testing.T) {
	b, store, def, _ := chatSkillFixture(t)
	chatDir := store.ChatDir("current")
	script := filepath.Join(chatDir, "work", "run.mjs")
	outside := filepath.Join(t.TempDir(), "outside.mjs")
	for _, p := range []string{script, outside} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("console.log(1)\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("console.log(1)\n"))
		data, _ := json.Marshal(map[string]any{"filePath": p, "sha256": hex.EncodeToString(sum[:])})
		ts := int64(1791777600002)
		if err := store.AppendStepLine("current", chat.StepLine{Type: chat.StepLineTypeReactTool, ChatID: "current", RunID: "first", UpdatedAt: ts,
			Messages: []chat.StoredMessage{{Role: "tool", Name: "file_write", ToolCallID: "call-" + filepath.Base(p), Ts: &ts,
				Content: []chat.ContentPart{{Type: "text", Text: string(data)}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	req := runtimetypes.QueryCommand{AgentKey: def.Key, ChatID: "current", RunID: "second", Message: "run it again"}
	s, err := b.BuildQuerySession(t.Context(), req, chat.Summary{ChatID: "current"}, def, Options{IncludeHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.ChatAuthoredFiles) != 1 || s.ChatAuthoredFiles[0].Path != script {
		t.Fatalf("inherited %+v, want only the Chat-directory script", s.ChatAuthoredFiles)
	}
	ctx := &contracts.ExecutionContext{Session: s}
	ctx.EnsureAuthoredScripts()
	if !ctx.AuthoredScripts.Matches(ctx.ScriptOwner(), script) {
		t.Fatal("new Run missed the script an earlier Run wrote")
	}
	if err := os.WriteFile(script, []byte("console.log(2)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if ctx.AuthoredScripts.Matches(ctx.ScriptOwner(), script) {
		t.Fatal("changed script kept its inherited proof")
	}

	// Side lanes, tasks and Team members start without the Chat's proofs.
	for name, tc := range map[string]struct {
		req     runtimetypes.QueryCommand
		options Options
	}{
		"side":    {runtimetypes.QueryCommand{ChatID: "current", RunID: "side", Lane: "btw"}, Options{IncludeHistory: true}},
		"task":    {req, Options{IncludeHistory: true, SubTaskID: "task"}},
		"member":  {req, Options{IncludeHistory: true, TeamHistoryAgentKey: "member"}},
		"history": {req, Options{}},
	} {
		session := s
		session.ChatAuthoredFiles = nil
		b.inheritChatAuthoredFiles(&session, tc.req, def, tc.options)
		if len(session.ChatAuthoredFiles) != 0 {
			t.Fatalf("%s inherited %+v", name, session.ChatAuthoredFiles)
		}
	}
}
