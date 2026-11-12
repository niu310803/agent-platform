package webapp

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type activeApp struct {
	state, generation string
	ctx               context.Context
	cancel            context.CancelFunc
	leases            sync.WaitGroup
	root              context.Context
	stop              context.CancelFunc
	done              chan struct{}
	clean             bool
	process           Process
	lastError         string
	restartCount      int
	exitCode          *int
}
type StateRequest struct {
	ExpectedRevision      string `json:"expectedRevision"`
	ExpectedStateRevision string `json:"expectedStateRevision"`
}

// Scope is an internal lease, never a public response or a request DTO.
type Scope struct {
	Entry              Entry
	Manifest           Manifest
	Policy             Policy
	UserConfig         map[string]any
	CodeRoot, DataRoot string
	InstallationID     string
	BackendAddress     string
	Generation         string
	Context            context.Context
	Check              func() error
	Release            func()
}

func (s *Service) Start(ctx context.Context, id string, request StateRequest) (Entry, error) {
	if !idPattern.MatchString(id) {
		return Entry{}, fail("webapp_not_found", 404)
	}
	control := &s.controlMu
	control.Lock()
	defer control.Unlock()
	return s.start(ctx, id, request)
}

// start requires the instance control lock.
func (s *Service) start(ctx context.Context, id string, request StateRequest) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mutationError(); err != nil {
		return Entry{}, err
	}
	r := s.recordFor(id)
	if r == nil {
		return Entry{}, fail("webapp_not_found", 404)
	}
	if request.ExpectedRevision != r.Revision {
		return Entry{}, fail("webapp_revision_conflict", 409)
	}
	if a := s.active; a != nil {
		if a.state == "running" || a.state == "starting" || a.state == "backoff" {
			return s.entry(r), nil
		}
		if !a.clean {
			return Entry{}, fail("webapp_transition_in_progress", 409)
		}
		s.active = nil
	}
	if request.ExpectedStateRevision != s.desired.Revision {
		return Entry{}, fail("webapp_revision_conflict", 409)
	}
	if r.Manifest.Backend != nil && (s.runner == nil || !s.runner.Ready()) {
		return Entry{}, fail("webapp_backend_unavailable", 503)
	}
	values, _, err := s.readUserConfig(id)
	if err != nil {
		return Entry{}, err
	}
	if err := validateUserValues(r.Manifest, values); err != nil {
		return Entry{}, err
	}
	if ctx.Err() != nil {
		return Entry{}, ctx.Err()
	}
	desired := Desired{SchemaVersion: 1, State: "running", Revision: randomID()}
	if err := writeJSON(filepath.Join(s.root, id, "desired.json"), desired); err != nil {
		return Entry{}, err
	}
	root, stop := context.WithCancel(context.Background())
	appCtx, cancel := context.WithCancel(root)
	a := &activeApp{state: "running", generation: randomID(), ctx: appCtx, cancel: cancel, root: root, stop: stop, done: make(chan struct{}), clean: true}
	s.active = a
	if r.Manifest.Backend != nil {
		a.state = "starting"
		a.clean = false
		go s.runBackend(id, a, *r, s.policy)
	} else {
		close(a.done)
	}
	s.desired = desired
	return s.entry(r), nil
}
func (s *Service) Stop(ctx context.Context, id string, request StateRequest) (Entry, error) {
	return s.stop(ctx, id, request, false)
}
func (s *Service) stop(ctx context.Context, id string, request StateRequest, preserve bool) (Entry, error) {
	if !idPattern.MatchString(id) {
		return Entry{}, fail("webapp_not_found", 404)
	}
	control := &s.controlMu
	control.Lock()
	defer control.Unlock()
	return s.stopLocked(ctx, id, request, preserve)
}
func (s *Service) stopLocked(ctx context.Context, id string, request StateRequest, preserve bool) (Entry, error) {
	s.mu.Lock()
	r := s.recordFor(id)
	if r == nil {
		s.mu.Unlock()
		return Entry{}, fail("webapp_not_found", 404)
	}
	if !preserve {
		if err := s.mutationError(); err != nil {
			s.mu.Unlock()
			return Entry{}, err
		}
		if r.Revision != request.ExpectedRevision {
			s.mu.Unlock()
			return Entry{}, fail("webapp_revision_conflict", 409)
		}
	}
	a := s.active
	if !preserve && (s.desired.State != "stopped" || a != nil) {
		if request.ExpectedStateRevision != s.desired.Revision {
			s.mu.Unlock()
			return Entry{}, fail("webapp_revision_conflict", 409)
		}
		d := Desired{SchemaVersion: 1, State: "stopped", Revision: randomID()}
		if err := writeJSON(filepath.Join(s.root, id, "desired.json"), d); err != nil {
			s.mu.Unlock()
			return Entry{}, err
		}
		s.desired = d
	}
	if a == nil {
		e := s.entry(r)
		s.mu.Unlock()
		return e, nil
	}
	a.state = "stopping"
	a.cancel()
	a.stop()
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { <-a.done; a.leases.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.mu.Lock()
		a.state = "failed"
		s.mu.Unlock()
		return Entry{}, fail("webapp_stop_incomplete", 503)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !a.clean {
		if checker, ok := s.runner.(interface{ ConfirmStopped(string) bool }); ok && checker.ConfirmStopped(id) {
			a.clean = true
		}
	}
	if !a.clean {
		a.state = "failed"
		return Entry{}, fail("webapp_stop_incomplete", 503)
	}
	if s.active == a {
		s.active = nil
	}
	return s.entry(r), nil
}
func (s *Service) Acquire(id string) (*Scope, error) { return s.acquire(id, "", "", false) }
func (s *Service) AcquireInstance(id, generation, policyRevision string) (*Scope, error) {
	return s.acquire(id, generation, policyRevision, true)
}
func (s *Service) acquire(id, expectedGeneration, expectedPolicy string, internal bool) (*Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mutationError(); err != nil {
		return nil, err
	}
	r := s.recordFor(id)
	if r == nil {
		return nil, fail("webapp_not_found", 404)
	}
	a := s.active
	if a == nil || a.ctx.Err() != nil || (!internal && a.state != "running") || (internal && (a.generation != expectedGeneration || s.policy.Revision != expectedPolicy || (a.state != "running" && a.state != "starting"))) {
		return nil, fail("webapp_not_running", 503)
	}
	values, _, err := s.readUserConfig(id)
	if err != nil {
		return nil, err
	}
	if err := validateUserValues(r.Manifest, values); err != nil {
		return nil, err
	}
	policy := s.policy
	a.leases.Add(1)
	var once sync.Once
	address := ""
	if a.process != nil {
		address = a.process.Address()
	}
	generation := a.generation
	appCtx := a.ctx
	return &Scope{InstallationID: r.InstallationID, BackendAddress: address, Entry: s.entry(r), Manifest: r.Manifest, Policy: policy, UserConfig: values, CodeRoot: s.codeRoot(id), DataRoot: s.appDataRoot(id), Generation: a.generation, Context: a.ctx, Release: func() { once.Do(a.leases.Done) }, Check: func() error {
		if appCtx.Err() != nil {
			return fail("app_grant_required", 403)
		}
		if internal {
			return s.instanceCheck(id, generation, policy.Revision)
		}
		return s.Check(id, generation, policy.Revision)
	}}, nil
}
func (s *Service) Check(id, generation, policyRevision string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mutationError(); err != nil {
		return err
	}
	a := s.active
	if s.recordFor(id) == nil || a == nil || a.state != "running" || a.generation != generation || a.ctx.Err() != nil || s.policy.Revision != policyRevision {
		return fail("app_grant_required", 403)
	}
	return nil
}
func (s *Service) Close() error {
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closing = true
	id := s.record.Manifest.ID
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	if _, err := s.stop(ctx, id, StateRequest{}, true); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return s.lock.Close()
}

// RestoreDesired starts only the package bound to this instance. Desired state
// is reset from that package at startup; installed application directories are
// never scanned or adopted.
func (s *Service) RestoreDesired(ctx context.Context) error {
	e := s.Current()
	_, err := s.Start(ctx, e.ID, StateRequest{ExpectedRevision: e.Revision, ExpectedStateRevision: e.StateRevision})
	return err
}

// CodeFile opens a file relative to the immutable frontend root with the Go
// root boundary, so later filesystem changes cannot turn a path into escape.
func (scope *Scope) CodeFile(name string) (*os.File, error) {
	if !safeRelative(name) {
		return nil, fail("webapp_path_invalid", 400)
	}
	root, err := os.OpenRoot(filepath.Join(scope.CodeRoot, scope.Manifest.Frontend.Root))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return root.Open(filepath.FromSlash(name))
}

func (s *Service) Restart(ctx context.Context, id string, req StateRequest) (Entry, error) {
	if !idPattern.MatchString(id) {
		return Entry{}, fail("webapp_not_found", 404)
	}
	l := &s.controlMu
	l.Lock()
	defer l.Unlock()
	e, err := s.stopLocked(ctx, id, req, false)
	if err != nil {
		return Entry{}, err
	}
	return s.start(ctx, id, StateRequest{ExpectedRevision: e.Revision, ExpectedStateRevision: e.StateRevision})
}
func (s *Service) runBackend(id string, a *activeApp, r Record, policy Policy) {
	defer close(a.done)
	var attempts []time.Time
	for {
		if a.root.Err() != nil {
			s.mu.Lock()
			a.clean = true
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		generation := a.generation
		values, _, configError := s.readUserConfig(id)
		appCtx := a.ctx
		s.mu.Unlock()
		spec := RuntimeSpec{ID: id, InstallationID: r.InstallationID, Generation: generation, CodeRoot: s.codeRoot(id), DataRoot: s.appDataRoot(id), ManagementRoot: filepath.Join(s.root, id), Manifest: r.Manifest, Policy: policy, UserConfig: values, Context: appCtx, Check: func() error { return s.instanceCheck(id, generation, policy.Revision) }}
		spec.OnUnavailable = func(reason string) {
			s.mu.Lock()
			if s.active == a && a.generation == generation && a.state != "stopping" {
				a.state = "unhealthy"
				a.lastError = reason
				a.cancel()
			}
			s.mu.Unlock()
		}

		var p Process
		err := configError
		if err == nil {
			p, err = s.runner.Start(appCtx, spec)
		}
		clean := true
		reason := ""
		code := 0
		started := time.Now()
		if err == nil {
			s.mu.Lock()
			a.process = p
			if a.root.Err() == nil {
				a.state = "running"
				a.lastError = ""
			}
			s.mu.Unlock()
			select {
			case <-p.Done():
			case <-a.root.Done():
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
			stopErr := p.Stop(stopCtx)
			cancel()
			result := p.Exit()
			clean = result.Clean && stopErr == nil
			reason = result.Reason
			code = result.Code
		} else {
			reason, _ = ErrorCode(err)
			if reason == "webapp_stop_incomplete" || reason == "webapp_previous_generation_alive" {
				clean = false
			}
		}
		s.mu.Lock()
		a.cancel()
		a.process = nil
		a.state = "backoff"
		a.exitCode = &code
		a.lastError = reason
		s.mu.Unlock()
		a.leases.Wait()
		s.mu.Lock()
		a.clean = clean
		if a.root.Err() != nil {
			s.mu.Unlock()
			return
		}
		if !clean || reason == "webapp_isolation_required" || reason == "webapp_backend_unavailable" {
			a.state = "blocked"
			s.mu.Unlock()
			return
		}
		now := time.Now()
		if now.Sub(started) >= time.Duration(s.config.Backend.Restart.StableResetSeconds)*time.Second {
			attempts = nil
		}
		cutoff := now.Add(-time.Duration(s.config.Backend.Restart.WindowSeconds) * time.Second)
		kept := attempts[:0]
		for _, t := range attempts {
			if t.After(cutoff) {
				kept = append(kept, t)
			}
		}
		attempts = kept
		if len(attempts) >= s.config.Backend.Restart.MaxAttempts {
			a.state = "failed"
			a.lastError = "webapp_restart_exhausted"
			s.mu.Unlock()
			return
		}
		attempts = append(attempts, now)
		a.restartCount++
		a.state = "backoff"
		s.mu.Unlock()
		delay := time.Second << min(len(attempts)-1, 5)
		timer := time.NewTimer(delay)
		select {
		case <-a.root.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		s.mu.Lock()
		if a.root.Err() != nil {
			s.mu.Unlock()
			return
		}
		a.generation = randomID()
		a.ctx, a.cancel = context.WithCancel(a.root)
		a.state = "starting"
		a.clean = false
		s.mu.Unlock()
	}
}
