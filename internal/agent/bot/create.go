package bot

import (
	"strings"

	"agent-platform/internal/contracts"
)

// DefaultWorkspaceRoot is written into a new BOT whose creator chose no
// Workspace. It names the host root working environment, not a project.
const DefaultWorkspaceRoot = "@root"

// ApplyCreateDefaults fills runtimeConfig.workspaceRoot only when the caller
// left it absent or blank. It runs on creation only, so existing BOT
// definitions keep whatever Workspace they have.
func ApplyCreateDefaults(definition map[string]any) map[string]any {
	if definition == nil {
		return nil
	}
	out := contracts.CloneMap(definition)
	runtimeConfig := contracts.CloneMap(contracts.AnyMapNode(out["runtimeConfig"]))
	if runtimeConfig == nil {
		runtimeConfig = map[string]any{}
	}
	if raw, exists := runtimeConfig["workspaceRoot"]; exists && raw != nil {
		// A non-string value is left for definition validation to reject.
		if value, ok := raw.(string); !ok || strings.TrimSpace(value) != "" {
			return out
		}
	}
	runtimeConfig["workspaceRoot"] = DefaultWorkspaceRoot
	out["runtimeConfig"] = runtimeConfig
	return out
}
