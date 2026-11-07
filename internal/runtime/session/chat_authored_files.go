package session

import (
	"log"

	"agent-platform/internal/accesspolicy"
	"agent-platform/internal/catalog"
	"agent-platform/internal/chat"
	"agent-platform/internal/contracts"
	runtimetypes "agent-platform/internal/runtime/types"
	"agent-platform/internal/scriptstate"
)

const maxChatAuthoredFiles = 64

// inheritChatAuthoredFiles lets a main-lane Run treat scripts that earlier
// Runs of this Chat wrote through the file tools like its own. The hashes come
// from platform-written tool results; each use still compares current bytes.
// Side lanes, tasks and remote backends start without them, as with skills.
func (b *Builder) inheritChatAuthoredFiles(session *contracts.QuerySession, req runtimetypes.QueryCommand, def catalog.AgentDefinition, options Options) {
	if !options.IncludeHistory || options.SubTaskID != "" || options.TeamHistoryAgentKey != "" || req.Lane != "" || remoteReferenceBackend(def) {
		return
	}
	reader, ok := b.deps.Chats.(chat.AuthoredFileReader)
	if !ok {
		return
	}
	files, err := reader.AuthoredFiles(req.ChatID)
	if err != nil {
		log.Printf("[session][chat-scripts] chat=%s run=%s: history unavailable: %v", req.ChatID, req.RunID, err)
		return
	}
	for _, f := range files {
		if len(session.ChatAuthoredFiles) == maxChatAuthoredFiles {
			break
		}
		if accesspolicy.PathInSessionChat(*session, f.Path) {
			session.ChatAuthoredFiles = append(session.ChatAuthoredFiles, scriptstate.Proof{Path: f.Path, SHA256: f.SHA256})
		}
	}
}
