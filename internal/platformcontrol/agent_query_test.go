package platformcontrol

import (
	"agent-platform/internal/catalog"
	"agent-platform/internal/config"
	"context"
	"testing"
)

func TestAgentQueryPrimaryDiscoveryAndEmptyPage(t *testing.T) {
	r := agentDiscoveryRegistry{items: []catalog.AdminAgent{
		{Key: "a", Name: "Other", Mode: "GENERAL"}, {Key: "b", Name: "BOT 示例", Description: "Report Bot", Mode: "BOT"}, {Key: "broken", Name: "Report Bot"},
	}, definitions: map[string]catalog.AgentDefinition{"a": {Key: "a"}, "b": {Key: "b", Mode: "BOT"}}}
	h := NewToolHandler(config.Config{}, r, nil)
	e := controlExecution()
	e.Session.PrimaryChat = true
	e.Session.Mode = "BOT"
	e.Session.NativeConnectorTools = nil
	e.Session.ConnectorDirs = nil
	result, err := h.Invoke(context.Background(), "agent_query", map[string]any{"action": "list", "args": map[string]any{"query": "REPORT", "limit": float64(1)}}, e)
	if err != nil || result.Error != "" {
		t.Fatalf("primary discovery rejected: %+v %v", result, err)
	}
	items := result.Structured["items"].([]any)
	if len(items) != 0 || result.Structured["nextCursor"] != "a" {
		t.Fatalf("lost empty-page cursor: %+v", result.Structured)
	}
	result, err = h.Invoke(context.Background(), "agent_query", map[string]any{"action": "list", "args": map[string]any{"query": "REPORT", "cursor": "a"}}, e)
	if err != nil || result.Error != "" {
		t.Fatalf("%+v %v", result, err)
	}
	items = result.Structured["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["agentKey"] != "b" || len(items[0].(map[string]any)) != 4 {
		t.Fatalf("wrong public summary: %+v", items)
	}
	e.Session.PrimaryChat = false
	result, _ = h.Invoke(context.Background(), "agent_query", map[string]any{"action": "list"}, e)
	if result.Error == "" {
		t.Fatal("unmounted ordinary Agent got discovery")
	}
}
