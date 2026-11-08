package platformcontrol

import (
	"context"
	"strings"
)

// Use the same published, valid directory as catalog_query. Filter after each
// directory page so the cursor retains the existing stable key contract.
func (h *ToolHandler) agentQuery(ctx context.Context, p map[string]any) (any, error) {
	args := map[string]any{"resourceType": "agent", "status": "valid"}
	for _, key := range []string{"limit", "cursor"} {
		if v, ok := p[key]; ok {
			args[key] = v
		}
	}
	value, err := h.catalogQuery(ctx, "list", args)
	if err != nil {
		return nil, err
	}
	page := value.(map[string]any)
	items := []map[string]any{}
	query := strings.ToLower(stringValue(p, "query"))
	for _, item := range page["items"].([]map[string]any) {
		key, name, description := stringValue(item, "key"), stringValue(item, "name"), stringValue(item, "description")
		if query != "" && !strings.Contains(strings.ToLower(key), query) && !strings.Contains(strings.ToLower(name), query) && !strings.Contains(strings.ToLower(description), query) {
			continue
		}
		items = append(items, map[string]any{"agentKey": key, "name": name, "description": description, "mode": item["mode"]})
	}
	// total from catalog refers to the unfiltered directory and would mislead.
	delete(page, "total")
	page["items"] = items
	return page, nil
}
