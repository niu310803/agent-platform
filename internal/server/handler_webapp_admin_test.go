package server

import (
	"agent-platform/internal/webapptest"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func webAppAdminServer(t *testing.T) *Server {
	t.Helper()
	service, _, _ := webapptest.Open(t, `{"schemaVersion":2,"id":"webapp-0123456789abcdef","key":"admin-test","label":"Admin","version":"1.0.0","target":"any","hosting":"platform","minPlatformVersion":"0.5.0","frontend":{"root":"frontend","index":"index.html"}}`, map[string]string{"frontend/index.html": "hello"})
	s := webAppBoundaryServer()
	s.deps.WebApp = service
	return s
}
func adminCall(s *Server, method, path, body, subject, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "https://demo.platform.example.com/api/admin/webapps"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer owner-token")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r = r.WithContext(WithPrincipal(r.Context(), &Principal{Subject: subject}))
	w := httptest.NewRecorder()
	s.handleWebAppAdmin(w, r)
	return w
}
func TestSingleManagementRequiresOwnerAndExactOrigin(t *testing.T) {
	s := webAppAdminServer(t)
	for _, subject := range []string{"", "other-owner"} {
		if w := adminCall(s, "GET", "/webapp-0123456789abcdef", "", subject, ""); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	for _, origin := range []string{"https://evil.example", "http://127.0.0.1:20200"} {
		if w := adminCall(s, "PUT", "/webapp-0123456789abcdef/user-config", "{}", "owner", origin); w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if w := adminCall(s, "GET", "/webapp-0123456789abcdef", "", "owner", ""); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestSingleManagementRetiredRoutesAreAbsent(t *testing.T) {
	s := webAppAdminServer(t)
	for _, tc := range []struct{ method, path string }{{"GET", ""}, {"POST", "/uploads"}, {"POST", "/preflight"}, {"POST", "/install"}, {"GET", "/operations"}, {"GET", "/operations/id"}, {"POST", "/webapp-0123456789abcdef/start"}, {"POST", "/webapp-0123456789abcdef/stop"}, {"POST", "/webapp-0123456789abcdef/restart"}, {"DELETE", "/webapp-0123456789abcdef"}} {
		w := adminCall(s, tc.method, tc.path, "{}", "owner", "")
		if w.Code != 404 {
			t.Fatalf("retired %s %s: %d %s", tc.method, tc.path, w.Code, w.Body.String())
		}
	}
}
func TestSingleManagementRejectsMalformedJSON(t *testing.T) {
	s := webAppAdminServer(t)
	for _, body := range []string{`{"Values":{}}`, `{"values":null}`, `{"values":{},"values":{}}`, `{} {}`} {
		if w := adminCall(s, "PUT", "/webapp-0123456789abcdef/user-config", body, "owner", ""); w.Code != 400 {
			t.Fatal(body, w.Code, w.Body.String())
		}
	}
	r := httptest.NewRequest(http.MethodPut, "https://demo.platform.example.com/api/admin/webapps/webapp-0123456789abcdef/user-config", strings.NewReader("{}"))
	r = r.WithContext(WithPrincipal(r.Context(), &Principal{Subject: "owner"}))
	w := httptest.NewRecorder()
	s.handleWebAppAdmin(w, r)
	if w.Code != 403 {
		t.Fatal("cookie write without Origin", w.Code)
	}
}
