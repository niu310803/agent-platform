package chat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// AuthoredFile is the content of a file last written through the managed file
// tools by this Chat's main lane.
type AuthoredFile struct {
	Path   string
	SHA256 string
}

var authoredSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// AuthoredFiles replays main-lane file_write/file_edit results in order and
// returns each file's last proven content, most recent first. A write proves
// its bytes; an edit only advances a proof whose previous hash it names, so an
// edit of content written another way drops the proof. Only tool results the
// platform recorded count; model messages and caller input are never read.
// The caller must still compare the file's current bytes.
func (s *FileStore) AuthoredFiles(chatID string) ([]AuthoredFile, error) {
	chatID = strings.TrimSpace(chatID)
	if !ValidChatID(chatID) {
		return nil, os.ErrPermission
	}
	// Like PriorSkillReferenceIDs, read without FileStore's write lock; a
	// concurrent partial append fails this optional lookup.
	lines, err := readPersistedJSONLines(s.chatJSONLPath(chatID))
	if err != nil {
		return nil, err
	}
	mainRuns := mainLaneRuns(lines)
	type proof struct {
		hash  string
		order int
	}
	proofs := map[string]proof{}
	order := 0
	for _, line := range lines {
		switch stringFromAny(line["_type"]) {
		case StepLineTypeReact, StepLineTypeReactTool:
		default:
			continue
		}
		// TEAM task rows may share the coordinator's Run ID.
		if !mainRuns[strings.TrimSpace(stringFromAny(line["runId"]))] ||
			strings.TrimSpace(stringFromAny(line["taskId"])) != "" || lineTaskAgentKey(line) != "" {
			continue
		}
		for _, raw := range anySlice(line["messages"]) {
			msg := anyMap(raw)
			name := stringFromAny(msg["name"])
			if stringFromAny(msg["role"]) != "tool" || name != "file_write" && name != "file_edit" {
				continue
			}
			result, ok := fileToolResult(msg["content"])
			if !ok {
				continue // failed calls leave the file unchanged
			}
			order++
			if old, exists := proofs[result.FilePath]; name == "file_edit" && (!exists || old.hash != result.PreviousSHA256) {
				delete(proofs, result.FilePath)
				continue
			}
			proofs[result.FilePath] = proof{hash: result.SHA256, order: order}
		}
	}
	files := make([]AuthoredFile, 0, len(proofs))
	for path, p := range proofs {
		files = append(files, AuthoredFile{Path: path, SHA256: p.hash})
	}
	sort.Slice(files, func(i, j int) bool { return proofs[files[i].Path].order > proofs[files[j].Path].order })
	return files, nil
}

type fileToolPayload struct {
	FilePath       string `json:"filePath"`
	SHA256         string `json:"sha256"`
	PreviousSHA256 string `json:"previousSha256"`
}

// fileToolResult reads the business result stored in a tool message's content.
func fileToolResult(content any) (fileToolPayload, bool) {
	var text strings.Builder
	for _, part := range anySlice(content) {
		text.WriteString(stringFromAny(anyMap(part)["text"]))
	}
	var result fileToolPayload
	if json.Unmarshal([]byte(text.String()), &result) != nil || !filepath.IsAbs(result.FilePath) || !authoredSHA256.MatchString(result.SHA256) {
		return fileToolPayload{}, false
	}
	return result, true
}
