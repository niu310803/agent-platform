package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPresetToolsConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
		bad   bool
	}{
		{"general: {}", nil, false}, {"preset-tools: []", []string{}, false},
		{"preset-tools:\n  - datetime\n  - wait\n  - datetime", []string{"datetime", "wait"}, false},
		{"preset-tools: wait", nil, true}, {"preset-tools: [1]", nil, true}, {"preset-tools: ['']", nil, true}, {"preset-tools: null", nil, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-settings.yml")
			_ = os.WriteFile(path, []byte(tc.value), 0600)
			var cfg Config
			err := cfg.applyAgentSettingsFile(path)
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v", err)
			}
			if !tc.bad && !reflect.DeepEqual(cfg.PresetTools, tc.want) {
				t.Fatalf("got %#v", cfg.PresetTools)
			}
		})
	}
}

func TestPresetConnectorsConfig(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  []string
		bad   bool
	}{
		{"preset-tools: []", nil, false}, {"preset-connectors: []", []string{}, false},
		{"preset-connectors:\n  - builtin.web-control\n  - builtin.web-control", []string{"builtin.web-control"}, false},
		{"preset-connectors: nope", nil, true}, {"preset-connectors: null", nil, true},
		{"preset-connectors: ['../bad']", nil, true}, {"preset-connectors: [1]", nil, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent-settings.yml")
			_ = os.WriteFile(path, []byte(tc.value), 0600)
			cfg := Config{PresetConnectors: []string{"stale"}}
			err := cfg.applyAgentSettingsFile(path)
			if (err != nil) != tc.bad {
				t.Fatalf("err=%v", err)
			}
			if !tc.bad && !reflect.DeepEqual(cfg.PresetConnectors, tc.want) {
				t.Fatalf("got %#v", cfg.PresetConnectors)
			}
		})
	}
}

func TestToolExclusionsConfigAndMerge(t *testing.T) {
	var cfg Config
	body := "exclude-tools: [wait, wait]\ngeneral:\n  exclude-tools: []\ncoder:\n  exclude-tools: [bash, wait]\nbot:\n  exclude-tools: [agent_invoke]\n"
	if err := cfg.applyAgentSettingsFile(configFixture(t, "agent-settings.yml", body)); err != nil {
		t.Fatal(err)
	}
	for mode, want := range map[string][]string{
		"GENERAL": {"wait"}, "CODER": {"wait", "bash"}, "BOT": {"wait", "agent_invoke"}, "KBASE": {"wait"},
	} {
		if got := cfg.PresetsForMode(mode).ExcludedTools; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s exclusions=%v want=%v", mode, got, want)
		}
	}
	for _, body := range []string{"exclude-tools: wait", "exclude-tools: null", "coder:\n  exclude-tools: [1]", "bot:\n  exclude-tools: ['']", "proxy:\n  exclude-tools: []"} {
		if err := cfg.applyAgentSettingsFile(configFixture(t, "agent-settings.yml", body)); err == nil {
			t.Fatalf("accepted %q", body)
		}
	}
	if err := cfg.applyAgentSettingsFile(configFixture(t, "agent-settings.yml", "general: {}")); err != nil || len(cfg.ExcludedTools) != 0 {
		t.Fatalf("stale exclusions: %v %v", cfg.ExcludedTools, err)
	}
}

func TestShippedSettingsOnlyGeneralOffersAgentInvoke(t *testing.T) {
	var cfg Config
	if err := cfg.applyAgentSettingsFile(ProjectFile("configs/agent-settings.example.yml")); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"general", "coder", "kbase", "team", "bot", "executor"} {
		p := cfg.PresetsForMode(mode)
		has := func(names []string, name string) bool {
			for _, n := range names {
				if n == name {
					return true
				}
			}
			return false
		}
		if has(p.Tools, "agent_invoke") != (mode == "general") || has(p.ExcludedTools, "agent_invoke") != (mode != "general") {
			t.Fatalf("%s policy: %+v", mode, p)
		}
		for _, name := range []string{"automation_query", "automation_manage"} {
			if has(p.ExcludedTools, name) {
				t.Fatalf("%s excludes %s", mode, name)
			}
		}
	}
}
