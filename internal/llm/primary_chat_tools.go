package llm

import (
	"agent-platform/internal/api"
	"agent-platform/internal/contracts"
)

// These descriptions apply only to the cloned, model-facing definitions of a
// scheduling Chat. Ordinary task-control sessions keep their catalog contract.
func configurePrimaryChatTool(def *api.ToolDetailResponse, session contracts.QuerySession) {
	if !session.ProvidesPrimaryTool(def.Name) {
		return
	}
	descriptions := map[string]string{
		"action_announce": "Show the user one short progress sentence before dispatching work or changing direction. Use the user's language, describe the concrete action, and do not expose internal reasoning or mail handoffs. This tool does not wait or execute the task.",
		"chat_send":       "Send a durable letter to your worker Chat or, when requested by the user, another Agent. Use a confirmed agentKey; delegate unknown Agent names to a worker with catalog_query, never guess keys. For another BOT, omit chatId and newChat to address its primary Chat. For your own workers, prefer chatId for continuing work; use newChat:true for unrelated work, an explicit separate Chat, or no suitable match. Follow the supplied Primary Chat protocol; no skill read is required.",
		"chat_submit":     "Answer a delivered worker ask using its exact Run and awaiting IDs. Approval is limited by your current live permission; follow the supplied Primary Chat protocol.",
		"chat_get_status": "Read a mail receipt or a Run you control. Use exactly one of mailId or runId; delivery=inserted grants no Run control. Follow the supplied Primary Chat protocol.",
		"chat_interrupt":  "Interrupt a Run you control because your letter started it. Follow the supplied Primary Chat protocol; an inserted letter grants no control.",
		"chat_query":      "Read authorized conversation summaries, visible history, artifacts or model options. Actions and argument shapes are provided in the Primary Chat protocol in the system prompt.",
	}
	def.Description = descriptions[def.Name]
	properties, _ := def.Parameters["properties"].(map[string]any)
	update := func(name, text string) {
		if property, ok := properties[name].(map[string]any); ok {
			property["description"] = text
		}
	}
	switch def.Name {
	case "chat_send":
		update("agentKey", "Use the confirmed target Agent key, not its display name. Omit for your own worker. For a user-requested message to another BOT, specify its confirmed key and omit chatId and newChat to use its primary Chat.")
		update("accessLevel", "Only with newChat:true. For a self-worker, default or omission inherits your live level at atomic acceptance and freezes it. Concrete levels cannot exceed your live level. Other Agents retain their target-default rules.")
		update("modelKey", "Only with newChat:true. Omission uses the target Agent model, including for a self-worker; it does not inherit the scheduling model. Discover modelKey values with chat_query models.")
		update("reasoningEffort", "Only with newChat:true. Omission uses the target Agent reasoning setting, including for a self-worker; it does not inherit the scheduling effort.")
	case "chat_query":
		update("args", "Arguments for the selected action; use the action shapes in the Primary Chat protocol in the system prompt.")
	}
}
