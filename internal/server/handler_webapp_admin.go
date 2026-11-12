package server

import (
	"io"
	"mime"
	"net/http"
	"strings"

	"agent-platform/internal/api"
	"agent-platform/internal/webapp"
)

func (s *Server) webAppOwner(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Set("Cache-Control", "no-store")
	p := PrincipalFromContext(r.Context())
	owners := s.deps.Config.Auth.AllowedSubjects
	if p == nil || p.Subject == "" || len(owners) != 1 || p.Subject != owners[0] {
		writeJSON(w, 403, api.Failure(403, "webapp_owner_required"))
		return false
	}
	origin := r.Header.Get("Origin")
	if origin != "" && origin != s.deps.Config.WebApp.PlatformOrigin || r.Method != "GET" && r.Header.Get("Authorization") == "" && origin != s.deps.Config.WebApp.PlatformOrigin {
		writeJSON(w, 403, api.Failure(403, "webapp_origin_not_allowed"))
		return false
	}
	if s.deps.WebApp == nil {
		writeJSON(w, 503, api.Failure(503, "webapp_host_unavailable"))
		return false
	}
	return true
}
func webAppResponse(w http.ResponseWriter, value any, err error) {
	if err != nil {
		code, status := webapp.ErrorCode(err)
		writeJSON(w, status, api.Failure(status, code, map[string]any{"code": code, "status": status, "retryable": false}))
		return
	}
	writeJSON(w, 200, api.Success(value))
}
func decodeWebAppRequest(r *http.Request, target any) error {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		return &webapp.Error{Code: "webapp_invalid_request", Status: 400}
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return &webapp.Error{Code: "webapp_invalid_request", Status: 400}
	}
	return webapp.DecodeRequest(raw, target)
}
func (s *Server) handleWebAppAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.webAppOwner(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/webapps")
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	if path == "/sessions/revoke" && r.Method == http.MethodPost && s.webAppGateway != nil {
		s.webAppGateway.RevokeSessions()
		webAppResponse(w, map[string]bool{"revoked": true}, nil)
		return
	}
	if len(parts) == 2 && parts[0] != "" {
		id := parts[0]
		switch parts[1] {
		case "logs":
			if r.Method != http.MethodGet {
				break
			}
			stream := r.URL.Query().Get("stream")
			if stream == "" {
				stream = "stdout"
			}
			value, err := s.deps.WebApp.Logs(id, stream, r.URL.Query().Get("cursor"))
			webAppResponse(w, value, err)
			return
		case "launch-ticket":
			if r.Method != http.MethodPost || s.webAppGateway == nil {
				break
			}
			var input struct {
				ReturnPath string `json:"returnPath"`
			}
			if err := decodeWebAppRequest(r, &input); err != nil {
				webAppResponse(w, nil, err)
				return
			}
			value, err := s.webAppGateway.Launch(PrincipalFromContext(r.Context()).Subject, id, input.ReturnPath)
			webAppResponse(w, value, err)
			return
		case "user-config":
			if r.Method != http.MethodPut {
				break
			}
			var input webapp.UserConfigRequest
			if err := decodeWebAppRequest(r, &input); err != nil {
				webAppResponse(w, nil, err)
				return
			}
			value, err := s.deps.WebApp.SaveUserConfig(id, input)
			webAppResponse(w, value, err)
			return
		case "policy":
			if r.Method != http.MethodPut {
				break
			}
			var input webapp.PolicyRequest
			if err := decodeWebAppRequest(r, &input); err != nil {
				webAppResponse(w, nil, err)
				return
			}
			value, err := s.deps.WebApp.SavePolicy(id, input)
			webAppResponse(w, value, err)
			return
		}
	}
	if len(parts) == 1 && parts[0] != "" {
		if r.Method == http.MethodGet {
			value, err := s.deps.WebApp.Configuration(parts[0])
			webAppResponse(w, value, err)
			return
		}

	}
	writeJSON(w, 404, api.Failure(404, "webapp_route_not_found"))
}
