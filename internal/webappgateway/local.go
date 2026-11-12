package webappgateway

import (
	"agent-platform/internal/webapp"
	"net"
	"net/http"
	"strconv"
	"time"
)

type localListener struct {
	host     string
	server   *http.Server
	listener net.Listener
}

func (g *Gateway) ensureListener(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return &webapp.Error{Code: "webapp_host_stopped", Status: 503}
	}
	if id != g.appID {
		return &webapp.Error{Code: "webapp_not_found", Status: 404}
	}
	if g.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(g.config.Frontend.ListenAddress, strconv.Itoa(g.config.Frontend.Port)))
	if err != nil {
		return err
	}
	host := net.JoinHostPort("127.0.0.1", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != host {
			http.NotFound(w, r)
			return
		}
		g.ServeHTTP(w, r)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	g.listener = &localListener{host: host, server: server, listener: listener}
	go func() { _ = server.Serve(listener) }()
	return nil
}
func (g *Gateway) Close() {
	g.mu.Lock()
	g.closed = true
	listener := g.listener
	g.listener = nil
	g.mu.Unlock()
	g.RevokeSessions()
	if listener != nil {
		_ = listener.server.Close()
	}
}
