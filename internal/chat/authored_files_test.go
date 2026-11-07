package chat

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestAuthoredFilesReplaysMainLaneFileToolResults(t *testing.T) {
	s, err := NewFileStoreAtStartup(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	const chatID = "authored"
	hash := func(c string) string { return strings.Repeat(c, 64) }
	query := func(runID, taskID, lane string) {
		t.Helper()
		if err := s.AppendQueryLine(chatID, QueryLine{Type: "query", ChatID: chatID, RunID: runID, UpdatedAt: testEpochMillis(1),
			TaskID: taskID, Lane: lane, Query: map[string]any{"message": "go"}}); err != nil {
			t.Fatal(err)
		}
	}
	tool := func(runID, taskID, name string, result map[string]any) {
		t.Helper()
		data, _ := json.Marshal(result)
		ts := testEpochMillis(2)
		if err := s.AppendStepLine(chatID, StepLine{Type: StepLineTypeReactTool, ChatID: chatID, RunID: runID, UpdatedAt: testEpochMillis(2), TaskID: taskID,
			Messages: []StoredMessage{{Role: "tool", Name: name, ToolCallID: "call", Ts: &ts, Content: []ContentPart{{Type: "text", Text: string(data)}}}}}); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, sha string) map[string]any { return map[string]any{"filePath": path, "sha256": sha} }
	edit := func(path, previous, sha string) map[string]any {
		return map[string]any{"filePath": path, "previousSha256": previous, "sha256": sha}
	}

	query("first", "", "")
	tool("first", "", "file_write", write("/chat/a.py", hash("a")))
	tool("first", "", "file_edit", edit("/chat/a.py", hash("a"), hash("b")))      // advances
	tool("first", "", "file_write", write("/chat/b.sh", hash("c")))               // proven
	tool("first", "", "file_edit", edit("/chat/b.sh", hash("9"), hash("d")))      // edit of other bytes: dropped
	tool("first", "", "file_edit", edit("/chat/never.sh", "", hash("e")))         // edit without a write
	tool("first", "", "bash", write("/chat/fake.sh", hash("f")))                  // not a file tool
	tool("first", "", "file_write", map[string]any{"error": "file_write_failed"}) // failed call
	tool("first", "", "file_write", write("relative.sh", hash("1")))              // malformed
	query("side", "", "btw")
	tool("side", "", "file_write", write("/chat/side.sh", hash("2")))
	query("task-run", "task", "")
	tool("task-run", "task", "file_write", write("/chat/task.sh", hash("3")))
	// TEAM task rows may share the coordinator's Run ID.
	tool("first", "member", "file_write", write("/chat/member.sh", hash("4")))
	tool("orphan", "", "file_write", write("/chat/orphan.sh", hash("5")))
	query("second", "", "")
	tool("second", "", "file_write", write("/chat/c.mjs", hash("6")))

	got, err := s.AuthoredFiles(chatID)
	want := []AuthoredFile{{Path: "/chat/c.mjs", SHA256: hash("6")}, {Path: "/chat/a.py", SHA256: hash("b")}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("files=%+v err=%v", got, err)
	}
	if _, err := s.AuthoredFiles("../outside"); !os.IsPermission(err) {
		t.Fatalf("invalid Chat path accepted: %v", err)
	}
}
