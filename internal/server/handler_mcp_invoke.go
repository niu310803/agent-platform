package server

import (
	"agent-platform/internal/api"
	"agent-platform/internal/remotemcp"
	"io"
	"net/http"
)

func writeRemoteMCPError(w http.ResponseWriter, err error) {
	code, status := remotemcp.ErrorCode(err)
	writeJSON(w, status, map[string]any{"code": status, "msg": code, "data": map[string]string{"errorCode": code}})
}
func (s *Server) handleMCPInvoke(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		writeRemoteMCPError(w, &remotemcp.Error{Code: "method_not_allowed", Status: 405})
		return
	}
	if _, err := connectorAuthoritySubject(r); err != nil {
		writeRemoteMCPError(w, &remotemcp.Error{Code: "mcp_authority_required", Status: 403})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, remotemcp.MaxInput+1))
	if err != nil || r.URL.RawQuery != "" {
		writeRemoteMCPError(w, &remotemcp.Error{Code: "invalid_arguments", Status: 400})
		return
	}
	input, err := remotemcp.Decode(raw)
	if err != nil {
		writeRemoteMCPError(w, err)
		return
	}
	if s.deps.RemoteMCP == nil {
		writeRemoteMCPError(w, &remotemcp.Error{Code: "mcp_unavailable", Status: 503})
		return
	}
	result, err := s.deps.RemoteMCP.Invoke(r.Context(), input)
	if err != nil {
		writeRemoteMCPError(w, err)
		return
	}
	writeJSON(w, 200, api.Success(result))
}
