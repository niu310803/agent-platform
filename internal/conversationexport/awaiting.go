package conversationexport

import (
	"cmp"
	"encoding/json"
	"fmt"
	"strings"

	"agent-platform/internal/i18n"
	"agent-platform/internal/stream"
)

func projectError(raw any, locale, fallback string) *ErrorV1 {
	value, _ := raw.(map[string]any)
	code := strings.TrimSpace(stringField(value, "code"))
	if code == "" {
		code = fallback
	}
	return &ErrorV1{Code: code, Message: i18n.ErrorMessage(locale, code)}
}

// The envelope matches the existing read-only AwaitingAnswerBlock. Only display
// fields are copied; raw request.submit and audit messages are not exported.
func projectAwaitingAnswer(event, definition stream.EventData, locale string) (string, error) {
	status := event.String("status")
	envelope := map[string]any{"status": status}
	if status == "error" {
		envelope["error"] = projectError(event.Value("error"), locale, "awaiting_error")
	} else if status == "answered" {
		mode := event.String("mode")
		var values []any
		var definitions []any
		switch mode {
		case "question":
			values, definitions = objectSlice(event.Value("answers")), objectSlice(definition.Value("questions"))
		case "approval":
			values, definitions = objectSlice(event.Value("approvals")), objectSlice(definition.Value("approvals"))
		case "form", "planning":
			values, definitions = []any{event.Value(mode)}, []any{definition.Value(mode)}
		default:
			return "", nil
		}
		items := make([]map[string]any, 0, len(values))
		for _, raw := range values {
			value, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			var meta map[string]any
			for _, rawMeta := range definitions {
				candidate, _ := rawMeta.(map[string]any)
				if mode == "form" || mode == "planning" || stringField(candidate, "id") == stringField(value, "id") {
					meta = candidate
					break
				}
			}
			item := map[string]any{}
			for _, field := range []string{"id", "question", "header", "description", "command", "title", "action", "planningId"} {
				if text := cmp.Or(stringField(value, field), stringField(meta, field)); text != "" {
					item[field] = text
				}
			}
			for _, field := range []string{"decision", "reason", "answer", "answers", "form", "data"} {
				if data, exists := value[field]; exists {
					item[field] = data
				}
			}
			if mode == "question" {
				for _, field := range []string{"answer", "answers"} {
					answer, exists := value[field]
					if !exists {
						continue
					}
					values, multiple := []any{answer}, false
					switch answers := answer.(type) {
					case []any:
						values, multiple = append([]any{}, answers...), true
					case []string:
						values, multiple = make([]any, len(answers)), true
						for i, text := range answers {
							values[i] = text
						}
					}
					for i, submitted := range values {
						if stringField(meta, "type") == "password" || stringField(value, "type") == "password" {
							values[i] = "••••••"
							continue
						}
						for _, rawOption := range objectSlice(meta["options"]) {
							option, _ := rawOption.(map[string]any)
							if label := stringField(option, "label"); label != "" && fmt.Sprint(option["value"]) == fmt.Sprint(submitted) {
								values[i] = label
								break
							}
						}
					}
					if multiple {
						if field == "answer" {
							delete(item, "answer")
						}
						item["answers"] = values
					} else {
						item[field] = values[0]
					}
				}
			}
			items = append(items, item)
		}
		if mode == "planning" && len(items) > 0 {
			envelope["planning"] = items[0]
		} else {
			envelope["items"] = items
		}
	} else {
		return "", nil
	}
	data, err := json.Marshal(envelope)
	return string(data), err
}
