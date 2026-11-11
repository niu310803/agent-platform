package server

import (
	"testing"

	"agent-platform/internal/config"
	"agent-platform/internal/runtime/runstate"
)

func TestSingleStatusCountsDeferredPlanning(t *testing.T) {
	deferred := runstate.NewDeferredAwaitingStore()
	deferred.Register(DeferredAwaiting{AwaitingID: "plan"})
	s := &Server{deps: Dependencies{Config: config.Config{Auth: config.AuthConfig{AllowedSubjects: []string{"owner"}}}, DeferredAwaitings: deferred}}
	// Status authorization requires a hosted application; test the counters and
	// route contract separately from the application fixture in internal/app.
	if deferred.Count() != 1 {
		t.Fatal("planning confirmation missing")
	}
	deferred.Remove("plan")
	if deferred.Count() != 0 {
		t.Fatal("consumed plan still waiting")
	}
	end := s.beginSingleRequest()
	if s.singleActivity.requests != 1 {
		t.Fatal("request not tracked")
	}
	end()
	if s.singleActivity.requests != 0 || s.singleActivity.last.IsZero() {
		t.Fatal("request did not settle")
	}
	for _, path := range []string{"/api/admin/agent", "/api/terminal/open", "/api/automation", "/api/memory", "/api/kbases", "/api/voice/ws"} {
		if singleRouteAllowed(path, "POST") {
			t.Fatalf("allowed %s", path)
		}
	}
	if !singleRouteAllowed("/api/admin/webapps/webapp-0123456789abcdef/launch-ticket", "POST") || !singleRouteAllowed("/api/query", "POST") {
		t.Fatal("Agent/launch surface disabled")
	}
}
