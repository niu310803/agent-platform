package webapp

import (
	"fmt"
	"os"
	"sync"

	"agent-platform/internal/config"
)

// SDKStateDir holds this instance's bridge ownership receipts.
const SDKStateDir = ".sdk"

type Options struct {
	Root, DataRoot, SingleRoot string
	Config                     config.WebAppConfig
	Runner                     Runner
	PlatformVersion            string
	InstanceLock               *os.File
}

// Service owns exactly one already installed application. Installation,
// upgrades and removal belong to the external host.
type Service struct {
	mu                         sync.Mutex
	closeMu                    sync.Mutex
	controlMu                  sync.Mutex
	root, dataRoot, singleRoot string
	config                     config.WebAppConfig
	lock                       *os.File
	closed, closing            bool
	record                     *Record
	desired                    Desired
	policy                     Policy
	active                     *activeApp
	runner                     Runner
}

func Open(o Options) (*Service, error) {
	if o.Root == "" || o.DataRoot == "" || o.SingleRoot == "" {
		return nil, fmt.Errorf("single WebApp code, state and data roots are required")
	}
	s := &Service{root: o.Root, dataRoot: o.DataRoot, config: o.Config, runner: o.Runner}
	return s.openSingle(o)
}

// recordFor requires mu. IDs identify the current package; they cannot select
// or create another application in this Platform instance.
func (s *Service) recordFor(id string) *Record {
	if s.record != nil && s.record.Manifest.ID == id {
		return s.record
	}
	return nil
}
func (s *Service) mutationError() error {
	if s.closed || s.closing {
		return fail("webapp_host_stopped", 503)
	}
	return nil
}
func (s *Service) Current() Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entry(s.record)
}
func (s *Service) entry(record *Record) Entry {
	m := record.Manifest
	state := s.desired.State
	observed := "stopped"
	generation := ""
	if a := s.active; a != nil {
		observed = a.state
		generation = a.generation
	}
	lastError := ""
	restartCount := 0
	var exitCode *int
	if a := s.active; a != nil {
		lastError = a.lastError
		restartCount = a.restartCount
		exitCode = a.exitCode
	}
	return Entry{LastError: lastError, RestartCount: restartCount, ExitCode: exitCode, ID: m.ID, Key: m.Key, Label: m.Label, Version: m.Version, Target: m.Target, Revision: record.Revision, StateRevision: s.desired.Revision, DesiredState: state, ObservedState: observed, Generation: generation, Backend: m.Backend != nil, InstalledAt: record.InstalledAt}
}
func (s *Service) Get(id string) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mutationError(); err != nil {
		return Entry{}, err
	}
	if r := s.recordFor(id); r != nil {
		return s.entry(r), nil
	}
	return Entry{}, fail("webapp_not_found", 404)
}

func defaultUserConfig(m Manifest) map[string]any {
	values := map[string]any{}
	if m.UserConfig != nil {
		for _, f := range m.UserConfig.Fields {
			if f.Default != nil {
				values[f.Name] = f.Default
			} else if f.Type == "boolean" {
				values[f.Name] = false
			}
		}
	}
	return values
}

func (s *Service) BackendAvailable() bool { return s.runner != nil && s.runner.Ready() }
