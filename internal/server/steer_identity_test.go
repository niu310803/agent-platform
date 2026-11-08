package server

import (
	"bufio"
	"bytes"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-platform/internal/api"
	"agent-platform/internal/config"
	"agent-platform/internal/ws"

	gws "github.com/gorilla/websocket"
)

// A message steer passes through Mailbox, whose Chat ownership check must see
// the same authenticated user that opened the Chat, over HTTP and WebSocket.
func TestAuthenticatedMessageSteerKeepsCallerIdentity(t *testing.T) {
	for _, transport := range []string{"http", "ws"} {
		t.Run(transport, func(t *testing.T) {
			const (
				chatID    = "chat-authenticated-steer"
				steerID   = "authenticated-steer-1"
				requestID = "authenticated-steer-request"
			)
			var calls atomic.Int32
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			var privateKey *rsa.PrivateKey
			fixture := newTestFixtureWithModelHandlerAndOptions(t, func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"draft"}}]}`)
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				writeProviderSSE(t, w, `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`, `[DONE]`)
			}, testFixtureOptions{notifications: ws.NewHub(), setupRuntime: func(root string, cfg *config.Config) {
				var publicKeyPath string
				privateKey, publicKeyPath = writeTestJWTKeyPair(t, root)
				cfg.Auth = config.AuthConfig{Enabled: true, LocalPublicKeyFile: publicKeyPath, Issuer: "agent-platform-local"}
			}})
			server := newLoopbackServer(t, fixture.server)
			defer server.Close()
			defer unblock()
			token := mustSignRS256JWT(t, privateKey, map[string]any{"sub": "tester", "iss": "agent-platform-local", "exp": float64(4102444800)})
			steer := api.SteerRequest{RequestID: requestID, ChatID: chatID, AgentKey: "mock-agent", SteerID: steerID, Message: "focus"}

			var ack api.SteerResponse
			if transport == "http" {
				req, _ := http.NewRequest(http.MethodPost, server.URL+"/api/query", strings.NewReader(`{"chatId":"`+chatID+`","agentKey":"mock-agent","message":"start"}`))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+token)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				reader := bufio.NewReader(resp.Body)
				for {
					line, readErr := reader.ReadString('\n')
					if strings.HasPrefix(line, "data: {") {
						event := decodeSSELine(t, line)
						if id, _ := event["runId"].(string); id != "" {
							steer.RunID = id
						}
						if event["delta"] == "draft" {
							break
						}
					}
					if readErr != nil {
						t.Fatalf("read query stream: %v", readErr)
					}
				}
				encoded, _ := json.Marshal(steer)
				rec := httptest.NewRecorder()
				steerReq := httptest.NewRequest(http.MethodPost, "/api/steer", bytes.NewReader(encoded))
				steerReq.Header.Set("Content-Type", "application/json")
				steerReq.Header.Set("Authorization", "Bearer "+token)
				fixture.server.ServeHTTP(rec, steerReq)
				var result struct {
					Data api.SteerResponse `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK {
					t.Fatalf("steer: %d %s %v", rec.Code, rec.Body.String(), err)
				}
				ack = result.Data
				unblock()
				if _, err := io.Copy(io.Discard, reader); err != nil {
					t.Fatal(err)
				}
			} else {
				conn, _, err := gws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/ws?token="+url.QueryEscape(token), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				readConnectedPush(t, conn)
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				write := func(id, route string, payload any) {
					if err := conn.WriteJSON(map[string]any{"frame": "request", "type": route, "id": id, "payload": payload}); err != nil {
						t.Fatal(err)
					}
				}
				write("query", "/api/query", map[string]any{"chatId": chatID, "agentKey": "mock-agent", "message": "start"})
				for {
					var frame ws.StreamFrame
					if err := conn.ReadJSON(&frame); err != nil {
						t.Fatal(err)
					}
					if frame.Event != nil {
						if id, ok := frame.Event.Value("runId").(string); ok && id != "" {
							steer.RunID = id
						}
						if frame.Event.Value("delta") == "draft" {
							break
						}
					}
				}
				write("steer", "/api/steer", steer)
				queryDone := false
				for {
					var frame map[string]json.RawMessage
					if err := conn.ReadJSON(&frame); err != nil {
						t.Fatal(err)
					}
					if string(frame["id"]) == `"query"` && len(frame["reason"]) > 0 {
						queryDone = true
					}
					if string(frame["id"]) != `"steer"` {
						continue
					}
					if string(frame["frame"]) != `"response"` {
						raw, _ := json.Marshal(frame)
						t.Fatalf("steer: %s", raw)
					}
					if err := json.Unmarshal(frame["data"], &ack); err != nil {
						t.Fatal(err)
					}
					break
				}
				unblock()
				for !queryDone {
					var frame map[string]json.RawMessage
					if err := conn.ReadJSON(&frame); err != nil {
						t.Fatal(err)
					}
					queryDone = string(frame["id"]) == `"query"` && len(frame["reason"]) > 0
				}
			}

			if !ack.Accepted || ack.Status != "accepted" || ack.RunID != steer.RunID || ack.SteerID != steerID {
				t.Fatalf("steer ack: %#v", ack)
			}
			summary, err := fixture.chats.Summary(chatID)
			if err != nil || summary == nil || summary.Source != "query:tester" {
				t.Fatalf("chat source: %#v %v", summary, err)
			}
			mail, err := fixture.server.deps.Mailbox.Store.UserRequest("tester", requestID)
			if err != nil || mail.Subject != "tester" || mail.RunID != steer.RunID || mail.SteerID != steerID {
				t.Fatalf("steer mail: %+v %v", mail, err)
			}
		})
	}
}
