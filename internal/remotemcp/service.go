// Package remotemcp executes trusted host requests against caller-selected HTTP
// MCP endpoints. It has no catalog, connector installation or Agent dependency.
package remotemcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"agent-platform/internal/agentconfig"
	"agent-platform/internal/httpclient"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const MaxInput = 256 << 10
const MaxOutput = 1 << 20

type Request struct {
	URL       string         `json:"url"`
	ToolName  string         `json:"toolName"`
	Arguments map[string]any `json:"arguments"`
	Auth      string         `json:"auth,omitempty"`
}
type Result struct {
	MCP *sdk.CallToolResult `json:"mcp"`
}
type Error struct {
	Code   string
	Status int
}

func (e *Error) Error() string           { return e.Code }
func fail(code string, status int) error { return &Error{code, status} }
func ErrorCode(err error) (string, int) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, e.Status
	}
	return "mcp_unavailable", 503
}

// Decode rejects duplicate, unknown and case-variant keys, trailing JSON and
// null fields. Arguments are opaque tool input, not host authentication options.
func Decode(raw []byte) (Request, error) {
	var r Request
	bad := func() (Request, error) { return r, fail("invalid_arguments", 400) }
	if len(raw) > MaxInput {
		return bad()
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return bad()
	}
	seen := map[string]bool{}
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return bad()
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return bad()
		}
		seen[key] = true
		var v json.RawMessage
		if d.Decode(&v) != nil || bytes.Equal(v, []byte("null")) {
			return bad()
		}
		switch key {
		case "url":
			err = json.Unmarshal(v, &r.URL)
		case "toolName":
			err = json.Unmarshal(v, &r.ToolName)
		case "arguments":
			args := json.NewDecoder(bytes.NewReader(v))
			args.UseNumber()
			err = args.Decode(&r.Arguments)
		case "auth":
			err = json.Unmarshal(v, &r.Auth)
			if r.Auth == "" {
				return bad()
			}
		default:
			return bad()
		}
		if err != nil {
			return bad()
		}
	}
	if _, err = d.Token(); err != nil {
		return bad()
	}
	if d.Decode(new(any)) != io.EOF {
		return bad()
	}
	if err = r.Validate(); err != nil {
		return r, err
	}
	return r, nil
}
func (r Request) Validate() error {
	u, err := url.Parse(r.URL)
	if err != nil || len(r.URL) > 8192 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "http" && u.Scheme != "https") || strings.TrimSpace(r.ToolName) == "" || len(r.ToolName) > 256 || r.Arguments == nil || (r.Auth != "" && r.Auth != "sso" && r.Auth != "none") {
		return fail("invalid_arguments", 400)
	}
	if r.Auth != "none" && u.Scheme != "https" {
		return fail("invalid_arguments", 400)
	}
	return nil
}

type Service struct {
	SsoIDTokenFile string
	HTTPClient     *http.Client
	// Timeout may shorten the default deadline for embedding and controlled tests.
	Timeout time.Duration
}

func (s *Service) Invoke(ctx context.Context, r Request) (*Result, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	input, err := json.Marshal(r)
	if err != nil || len(input) > MaxInput {
		return nil, fail("invalid_arguments", 400)
	}
	timeout := s.Timeout
	if timeout <= 0 || timeout > 30*time.Second {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if r.Auth != "none" {
		if token, e := agentconfig.ReadSsoIdTokenFile(s.SsoIDTokenFile); e != nil || token == "" {
			return nil, fail("sso_identity_unavailable", 401)
		}
	}
	base := s.HTTPClient
	if base == nil {
		base = httpclient.NewClient(0)
	}
	client := *base
	rt := base.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	transport := &boundedTransport{base: rt, ctx: ctx, identityFile: s.SsoIDTokenFile, sso: r.Auth != "none"}
	client.Transport = transport
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }
	c := sdk.NewClient(&sdk.Implementation{Name: "agent-platform", Version: "1"}, &sdk.ClientOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	session, err := c.Connect(ctx, &sdk.StreamableClientTransport{Endpoint: r.URL, HTTPClient: &client, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	classify := func(err error, calling bool) error {
		if transport.large.Load() {
			return fail("mcp_result_too_large", 413)
		}
		if transport.identityFailed.Load() {
			return fail("sso_identity_unavailable", 401)
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fail("mcp_timeout", 504)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return fail("mcp_canceled", 408)
		}
		if !calling {
			return fail("mcp_connect_failed", 502)
		}
		var wire *jsonrpc.Error
		if errors.As(err, &wire) && wire.Code != -32005 && wire.Code != -32003 {
			return fail("mcp_call_failed", 502)
		}
		return fail("mcp_outcome_unknown", 502)
	}
	if err != nil {
		return nil, classify(err, false)
	}
	defer session.Close()
	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: r.ToolName, Arguments: r.Arguments})
	if err != nil {
		return nil, classify(err, true)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, fail("mcp_call_failed", 502)
	}
	if len(raw) > MaxOutput {
		return nil, fail("mcp_result_too_large", 413)
	}
	return &Result{MCP: result}, nil
}

type boundedTransport struct {
	base           http.RoundTripper
	ctx            context.Context
	identityFile   string
	sso            bool
	total          atomic.Int64
	large          atomic.Bool
	identityFailed atomic.Bool
}

func (t *boundedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// SDK cleanup/cancellation requests may use detached contexts. Bind them too.
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	r = r.Clone(ctx)
	r.Header = r.Header.Clone()
	if t.sso {
		token, err := agentconfig.ReadSsoIdTokenFile(t.identityFile)
		if err != nil || token == "" {
			stop()
			cancel()
			t.identityFailed.Store(true)
			return nil, fail("sso_identity_unavailable", 401)
		}
		r.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := t.base.RoundTrip(r)
	if err != nil {
		stop()
		cancel()
		return nil, err
	}
	res.Body = &boundedBody{ReadCloser: res.Body, t: t, stop: stop, cancel: cancel}
	return res, nil
}

type boundedBody struct {
	io.ReadCloser
	t      *boundedTransport
	stop   func() bool
	cancel context.CancelFunc
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if len(p) > MaxOutput+1 {
		p = p[:MaxOutput+1]
	}
	n, err := b.ReadCloser.Read(p)
	if b.t.total.Add(int64(n)) > MaxOutput {
		b.t.large.Store(true)
		return 0, fail("mcp_result_too_large", 413)
	}
	return n, err
}
func (b *boundedBody) Close() error { b.stop(); b.cancel(); return b.ReadCloser.Close() }
