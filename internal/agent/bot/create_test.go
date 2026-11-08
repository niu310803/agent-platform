package bot

import (
	"testing"

	"agent-platform/internal/contracts"
)

func TestApplyCreateDefaultsFillsOnlyMissingWorkspace(t *testing.T) {
	for name, tc := range map[string]struct {
		definition map[string]any
		want       any
	}{
		"absent runtimeConfig": {map[string]any{"mode": Mode}, DefaultWorkspaceRoot},
		"absent workspaceRoot": {map[string]any{"runtimeConfig": map[string]any{"env": map[string]any{"A": "1"}}}, DefaultWorkspaceRoot},
		"blank workspaceRoot":  {map[string]any{"runtimeConfig": map[string]any{"workspaceRoot": "  "}}, DefaultWorkspaceRoot},
		"null workspaceRoot":   {map[string]any{"runtimeConfig": map[string]any{"workspaceRoot": nil}}, DefaultWorkspaceRoot},
		"explicit directory":   {map[string]any{"runtimeConfig": map[string]any{"workspaceRoot": "/srv/bot"}}, "/srv/bot"},
		"non-string value":     {map[string]any{"runtimeConfig": map[string]any{"workspaceRoot": 7}}, 7},
	} {
		t.Run(name, func(t *testing.T) {
			before := contracts.CloneMap(contracts.AnyMapNode(tc.definition["runtimeConfig"]))
			out := ApplyCreateDefaults(tc.definition)
			if got := contracts.AnyMapNode(out["runtimeConfig"])["workspaceRoot"]; got != tc.want {
				t.Fatalf("workspaceRoot=%#v, want %#v", got, tc.want)
			}
			if after := contracts.AnyMapNode(tc.definition["runtimeConfig"]); len(after) != len(before) || after["workspaceRoot"] != before["workspaceRoot"] {
				t.Fatalf("input runtimeConfig was modified: %#v", after)
			}
		})
	}
	if env := contracts.AnyMapNode(contracts.AnyMapNode(ApplyCreateDefaults(map[string]any{"runtimeConfig": map[string]any{"env": map[string]any{"A": "1"}}})["runtimeConfig"])["env"]); env["A"] != "1" {
		t.Fatalf("other runtimeConfig fields must be kept: %#v", env)
	}
	if ApplyCreateDefaults(nil) != nil {
		t.Fatal("nil definition must stay nil")
	}
}
