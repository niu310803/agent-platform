package server

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"agent-platform/internal/api"
)

type singleActivity struct {
	mu       sync.Mutex
	last     time.Time
	requests int
}

func (s *Server) beginSingleRequest() func() {
	a := &s.singleActivity
	a.mu.Lock()
	a.last = time.Now()
	a.requests++
	a.mu.Unlock()
	return func() { a.mu.Lock(); a.requests--; a.last = time.Now(); a.mu.Unlock() }
}
func (s *Server) handleSingleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.NotFound(w, r)
		return
	}
	if !s.webAppOwner(w, r) {
		return
	}
	a := &s.singleActivity
	a.mu.Lock()
	if a.last.IsZero() {
		a.last = time.Now()
	}
	last, requests := a.last.UnixMilli(), a.requests
	a.mu.Unlock()
	running, awaiting := 0, 0
	if counts, ok := s.deps.Runs.(interface{ Activity() (int, int) }); ok {
		running, awaiting = counts.Activity()
	}
	if counter, ok := s.deps.DeferredAwaitings.(interface{ Count() int }); ok {
		awaiting += counter.Count()
	}
	writeJSON(w, 200, api.Success(map[string]any{"lastRequestAt": last, "activeRequests": requests, "activeRuns": running, "awaitingRuns": awaiting, "idle": requests == 0 && running == 0 && awaiting == 0}))
}

// The single profile publishes only the Agent/Chat surface and its own launch
// flow. Apply the same boundary to WS routes before invoking their handlers.
func singleRouteAllowed(path, method string) bool {
	if strings.HasPrefix(path, "/api/admin/webapps") {
		return method == "GET" || method == "POST" && strings.HasSuffix(path, "/launch-ticket") || method == "PUT" && (strings.HasSuffix(path, "/user-config") || strings.HasSuffix(path, "/policy"))
	}
	if strings.HasPrefix(path, "/api/admin/") {
		for _, prefix := range []string{"/api/admin/agent", "/api/admin/skill", "/api/admin/registries", "/api/admin/source", "/api/admin/connector"} {
			if strings.HasPrefix(path, prefix) {
				return method == "GET"
			}
		}
		return false
	}
	for _, prefix := range []string{"/api/automation", "/api/mail", "/api/terminal", "/api/voice", "/api/kbase", "/api/memory", "/api/channel", "/api/gateway", "/api/desktop/"} {
		if strings.HasPrefix(path, prefix) {
			return false
		}
	}
	for _, p := range []string{"/api/agent/model-config", "/api/agents/connectors", "/api/agents/order", "/api/skills/order", "/api/agent/open-directory", "/api/connectors/order", "/api/send"} {
		if path == p && method != "GET" {
			return false
		}
	}
	return true
}

// PrepareSingleOrigin runs after binding the API listener, before accepting
// requests. Gateway creation is explicit so ready.json has an application URL.
func (s *Server) PrepareSingleOrigin(origin string) (string, error) {
	if !s.deps.Config.SingleWebApp() || s.webAppGateway == nil {
		return "", fmt.Errorf("single WebApp unavailable")
	}
	s.deps.Config.WebApps.PlatformOrigin = origin
	s.webAppGateway.SetPlatformOrigin(origin)
	apps := s.deps.WebApps.List()
	if len(apps) != 1 {
		return "", fmt.Errorf("single WebApp requires one application")
	}
	if err := s.webAppGateway.Prepare(apps[0].ID); err != nil {
		return "", err
	}
	return s.webAppGateway.Origin(apps[0].ID), nil
}
