package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"agent-platform/internal/catalog"
	"agent-platform/internal/config"
	"agent-platform/internal/webapp"
)

func validateSingleCatalog(apps *webapp.Service, registry *catalog.FileRegistry, cfg config.Config) error {
	entry := apps.Current()
	scope, err := apps.Configuration(entry.ID)
	if err != nil {
		return err
	}
	if scope.Manifest.Copilot == nil {
		return nil
	}
	def, ok := registry.AgentDefinition(scope.Manifest.Copilot.AgentKey)
	if !ok {
		return fmt.Errorf("packaged copilot Agent is unavailable")
	}
	for _, ref := range scope.Manifest.Copilot.References {
		if !strings.HasPrefix(ref.ID, "builtin.") && !strings.HasPrefix(ref.ID, "builtin/") {
			if _, err := os.Stat(filepath.Join(cfg.Paths.SkillsCenterDir, filepath.FromSlash(ref.ID), "SKILL.md")); err != nil {
				return fmt.Errorf("copilot skill %s must be packaged: %w", ref.ID, err)
			}
		}
		if _, ok := registry.SkillDefinition(ref.ID); ok {
			continue
		}
		if _, ok, err := def.ResolveSkillDefinition(ref.ID); err != nil || !ok {
			return fmt.Errorf("copilot skill %s is unavailable", ref.ID)
		}
	}
	return nil
}
