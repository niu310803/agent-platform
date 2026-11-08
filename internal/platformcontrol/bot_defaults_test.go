package platformcontrol

import (
	"testing"

	"agent-platform/internal/config"
	"agent-platform/internal/contracts"
)

func TestCatalogBotDefaultsUseWorkerConfiguration(t *testing.T) {
	for _, model := range []string{"worker-model", ""} {
		t.Run("model="+model, func(t *testing.T) {
			cfg := config.Config{
				Bot:             config.BotConfig{ModelKey: "scheduler-model"},
				GeneralSettings: config.GeneralSettingsConfig{DefaultAgent: config.CoderDefaultAgentConfig{ModelKey: "general-model"}},
				BotSettings: config.GeneralSettingsConfig{DefaultAgent: config.CoderDefaultAgentConfig{
					ModelKey: model, ReasoningEffort: "HIGH", Budget: map[string]any{"maxSteps": 200},
				}},
			}
			h := NewToolHandler(cfg, nil, nil)
			result := discoveryQuery(t, h, "defaults", map[string]any{"type": "bot"})
			definition := contracts.AnyMapNode(result["definitionDefaults"])
			modelConfig := contracts.AnyMapNode(definition["modelConfig"])
			if result["path"] != BotCreationPath || definition["mode"] != "BOT" || contracts.AnyStringNode(modelConfig["modelKey"]) != model {
				t.Fatalf("unexpected BOT defaults: %#v", result)
			}
			if contracts.AnyMapNode(modelConfig["reasoning"])["effort"] != "HIGH" || contracts.AnyMapNode(definition["budget"])["maxSteps"] != float64(200) {
				t.Fatalf("worker settings lost: %#v", result)
			}
			if result["ready"] != (model != "") {
				t.Fatalf("scheduler/general model must not fill missing worker model: %#v", result)
			}
			if _, exists := definition["connectorConfig"]; exists {
				t.Fatal("creation defaults must not implicitly grant connectors")
			}
		})
	}
}
