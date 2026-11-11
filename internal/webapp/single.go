package webapp

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/mod/semver"
)

func (s *Service) codeRoot(id string) string {
	if s.singleRoot != "" {
		return s.singleRoot
	}
	return filepath.Join(s.root, id, "app")
}
func (s *Service) appDataRoot(id string) string {
	if s.singleRoot != "" {
		return s.dataRoot
	}
	return filepath.Join(s.dataRoot, id)
}

// ClaimSingleInstance must precede stores and generated catalog initialization.
func ClaimSingleInstance(root string) (*os.File, error) {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	if canonical != root {
		return nil, fmt.Errorf("single WebApp root must be canonical")
	}
	dir := filepath.Join(root, ".lock", "webapps")
	if err := ensureDirectory(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := acquireInstance(f); err != nil {
		f.Close()
		return nil, fail("webapp_host_already_running", 409)
	}
	return f, nil
}

// Open the already installed flat application. Package installation and upgrades
// belong to the host; only private management records are written here.
func (s *Service) openSingle(o Options) (*Service, error) {
	root, err := filepath.Abs(o.SingleRoot)
	if err != nil {
		return nil, err
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil || canonical != root {
		return nil, fmt.Errorf("single WebApp root must be canonical")
	}
	raw, err := os.ReadFile(filepath.Join(root, "webapp.json"))
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	if m.Hosting != "platform" {
		return nil, fmt.Errorf("single WebApp requires hosting=platform")
	}
	minimum := "v" + strings.TrimPrefix(m.MinPlatformVersion, "v")
	current := "v" + strings.TrimPrefix(o.PlatformVersion, "v")
	if !semver.IsValid(minimum) || !semver.IsValid(current) || semver.Compare(current, minimum) < 0 {
		return nil, fmt.Errorf("WebApp requires Platform %s (current %s)", m.MinPlatformVersion, o.PlatformVersion)
	}
	if m.Target != "any" || m.Backend != nil && m.Backend.Command.Type != "node" {
		return nil, fail("webapp_launcher_unsupported", 400)
	}
	if err := validateBackendEnv(m); err != nil {
		return nil, err
	}
	s.singleRoot = root
	if checker, ok := s.runner.(interface{ ConfirmStopped(string) bool }); ok {
		if err := ensureDirectory(filepath.Join(s.root, m.ID)); err != nil {
			return nil, err
		}
		if !checker.ConfirmStopped(m.ID) {
			return nil, fail("webapp_previous_generation_alive", 409)
		}
	}
	for _, dir := range []string{s.root, s.dataRoot, filepath.Join(root, ".lock", "webapps"), filepath.Join(s.root, m.ID)} {
		if err := ensureDirectory(dir); err != nil {
			return nil, err
		}
	}
	s.lock = o.InstanceLock
	if s.lock == nil {
		s.lock, err = ClaimSingleInstance(root)
		if err != nil {
			return nil, err
		}
	}
	ok := false
	defer func() {
		if !ok {
			s.lock.Close()
		}
	}()
	// No package root grant: validate each entry through os.Root and reject links.
	for _, name := range []string{"webapp.json", m.Frontend.Root} {
		if err := singleItem(root, name); err != nil {
			return nil, err
		}
	}
	if m.Backend != nil {
		if err := singleItem(root, m.Backend.Command.Entry); err != nil {
			return nil, err
		}
	}
	if m.Copilot != nil {
		if err := singleItem(root, filepath.Join("agents", m.Copilot.AgentKey, "agent.yml")); err != nil {
			return nil, fmt.Errorf("copilot Agent must be packaged: %w", err)
		}
	}
	record := Record{SchemaVersion: 1, Manifest: m, Revision: randomID(), InstallationID: randomID(), InstalledAt: time.Now().UnixMilli()}
	metadata := filepath.Join(s.root, m.ID, "install.json")
	var previous Record
	if err := readJSON(metadata, &previous); err == nil {
		if previous.Manifest.ID != m.ID || !opaquePattern.MatchString(previous.InstallationID) {
			return nil, fmt.Errorf("invalid single WebApp installation identity")
		}
		record.InstallationID = previous.InstallationID
		record.InstalledAt = previous.InstalledAt
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := writeJSON(metadata, record); err != nil {
		return nil, err
	}
	s.records[m.ID] = &record
	s.desired[m.ID] = Desired{SchemaVersion: 1, State: "running", Revision: randomID()}
	policy := Policy{SchemaVersion: 1, Revision: randomID(), Skills: []string{}, Connectors: []ConnectorScope{}, ConnectPorts: []int{}}
	if m.Copilot != nil {
		policy.AgentKey = m.Copilot.AgentKey
		policy.Assistant = true
		policy.Artifacts = true
		for _, ref := range m.Copilot.References {
			policy.Skills = append(policy.Skills, ref.ID)
		}
	}
	policyFile := filepath.Join(s.root, m.ID, "policy.json")
	var previousPolicy Policy
	if err := readJSON(policyFile, &previousPolicy); err == nil {
		if err := validatePolicy(previousPolicy); err != nil {
			return nil, err
		}
		policy.ConnectPorts = previousPolicy.ConnectPorts
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := writeJSON(policyFile, policy); err != nil {
		return nil, err
	}
	s.policies[m.ID] = policy
	configFile := filepath.Join(s.root, m.ID, "user-config.json")
	if _, err := os.Stat(configFile); os.IsNotExist(err) {
		if err = writeJSON(configFile, defaultUserConfig(m)); err != nil {
			return nil, err
		}
	}
	ok = true
	return s, nil
}

func singleItem(root, name string) error {
	p := filepath.Join(root, name)
	canonical, err := filepath.EvalSymlinks(p)
	if err != nil {
		return err
	}
	if canonical != p {
		return fmt.Errorf("package symlinks are forbidden: %s", name)
	}
	return filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 || !d.IsDir() && !d.Type().IsRegular() {
			return fmt.Errorf("invalid package entry: %s", path)
		}
		return nil
	})
}

func validateBackendEnv(m Manifest) error {
	if m.Backend == nil {
		return nil
	}
	for k := range m.Backend.Env {
		u := strings.ToUpper(k)
		if u == "HOST" || u == "PORT" || strings.HasPrefix(u, "WEBAPP_") || strings.HasPrefix(u, "DESKTOP_") || u == "NODE_OPTIONS" || u == "NODE_PATH" || u == "HOME" || u == "PATH" || u == "TMP" || u == "TEMP" || u == "TMPDIR" || u == "SYSTEMROOT" || strings.HasPrefix(u, "ELECTRON_") || strings.HasPrefix(u, "LD_") || strings.HasPrefix(u, "DYLD_") || strings.HasPrefix(u, "AP_") {
			return fail("webapp_backend_env_forbidden", 400)
		}
	}
	return nil
}
