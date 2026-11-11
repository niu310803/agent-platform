package conversationexport

import (
	"fmt"
	"strings"

	"agent-platform/internal/i18n"
)

func RenderMarkdown(snapshot SnapshotV1) ([]byte, error) {
	var out strings.Builder
	text := func(key string) string { return i18n.ConversationExportText(snapshot.Locale, key) }
	for _, turn := range snapshot.Turns {
		if len(turn.Nodes) == 0 || turn.Nodes[0].Kind != "message" || turn.Nodes[0].Role != "user" {
			continue
		}
		var paragraphs []string
		for _, node := range turn.Nodes[1:] {
			if node.Kind == "content" && node.Role == "assistant" && node.TaskID == "" && strings.TrimSpace(node.Text) != "" {
				paragraphs = append(paragraphs, node.Text)
			}
		}
		if turn.Outcome == OutcomeCompleted && len(paragraphs) > 0 {
			paragraphs = paragraphs[len(paragraphs)-1:]
		}
		answer := strings.Join(paragraphs, "\n\n")
		if strings.TrimSpace(answer) == "" {
			answer = text("empty")
		}
		if out.Len() > 0 {
			out.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&out, "## %s\n\n", text("question"))
		out.WriteString(turn.Nodes[0].Text)
		for _, steer := range turn.Nodes[1:] {
			if steer.Kind != "message" || steer.Role != "user" || steer.SteerID == "" || strings.TrimSpace(steer.Text) == "" {
				continue
			}
			fmt.Fprintf(&out, "\n\n### %s\n\n", text("steer"))
			out.WriteString(steer.Text)
			if origin := steer.Origin; origin != nil {
				fmt.Fprintf(&out, "\n\n"+text("origin"), origin.AgentKey, origin.ChatID, origin.RunID, origin.ToolID)
			}
		}
		fmt.Fprintf(&out, "\n\n## %s\n\n", text("answer"))
		if turn.Outcome != OutcomeCompleted {
			fmt.Fprintf(&out, "> %s\n\n", text(string(turn.Outcome)))
		}
		out.WriteString(answer)
		out.WriteString("\n")
		if turn.Outcome == OutcomeFailed {
			err := turn.Error
			if err == nil {
				err = projectError(nil, snapshot.Locale, "run_error")
			}
			fmt.Fprintf(&out, "\n> %s (`%s`)\n", err.Message, err.Code)
		}
	}
	if out.Len() == 0 {
		return nil, ErrNoRootTurn
	}
	return []byte(out.String()), nil
}
