package session

import (
	"agent-platform/internal/catalog"
	"agent-platform/internal/chat"
	"agent-platform/internal/config"
	"agent-platform/internal/contracts"
	runtimetypes "agent-platform/internal/runtime/types"
	"testing"
)

type liveChatSummaries struct {
	workerRoleHistory
	items map[string]chat.Summary
}

func (s *liveChatSummaries) Summary(id string) (*chat.Summary, error) {
	v, ok := s.items[id]
	if !ok {
		return nil, nil
	}
	return &v, nil
}
func TestBotChatReadGrantChecksLiveAgentOwnerAndArchive(t *testing.T) {
	store := &liveChatSummaries{items: map[string]chat.Summary{"peer": {ChatID: "peer", AgentKey: "bot", Source: "query:owner"}}}
	b := New(Dependencies{Chats: store, Config: config.Config{Paths: config.PathsConfig{ChatsDir: t.TempDir()}}})
	def := catalog.AgentDefinition{Key: "bot", Mode: "BOT"}
	build := func(subject string) contracts.QuerySession {
		s, err := b.BuildQuerySession(t.Context(), runtimetypes.QueryCommand{ChatID: "current", AgentKey: "bot"}, chat.Summary{ChatID: "current"}, def, Options{Identity: &contracts.AuthIdentity{Subject: subject}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := build("owner")
	if s.CanReadChatFiles == nil || !s.CanReadChatFiles("peer") {
		t.Fatal("peer read not granted")
	}
	store.items["new-worker"] = chat.Summary{ChatID: "new-worker", AgentKey: "bot", Source: "query:owner"}
	if !s.CanReadChatFiles("new-worker") {
		t.Fatal("worker created during Run invisible")
	}
	for _, v := range []chat.Summary{{ChatID: "peer", AgentKey: "other", Source: "query:owner"}, {ChatID: "peer", AgentKey: "bot", Source: "query:another"}} {
		store.items["peer"] = v
		if s.CanReadChatFiles("peer") {
			t.Fatal("foreign Agent/owner granted")
		}
	}
	delete(store.items, "peer")
	if s.CanReadChatFiles("peer") {
		t.Fatal("archived/missing Chat still granted")
	}
	if anonymous := build(""); anonymous.CanReadChatFiles != nil {
		t.Fatal("anonymous exception granted")
	}
	def.Mode = "GENERAL"
	if ordinary := build("owner"); ordinary.CanReadChatFiles != nil {
		t.Fatal("BOT exception granted to ordinary Agent")
	}
}
