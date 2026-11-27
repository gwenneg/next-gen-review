package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

func TestHealth(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		dbErr  error
		status int
		body   string
	}{
		{"database up", "127.0.0.1:8080", nil, http.StatusOK, `"database":"ok"`},
		{"database up, localhost", "localhost:8080", nil, http.StatusOK, `"database":"ok"`},
		{"database down", "127.0.0.1:8080", errors.New("connection refused"), http.StatusServiceUnavailable, `"database":"unreachable"`},
		{"dns rebinding", "attacker.example:8080", nil, http.StatusForbidden, "host not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			New(fakeDB{tt.dbErr}).ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if !strings.Contains(rec.Body.String(), tt.body) {
				t.Errorf("body = %s, want it to contain %s", rec.Body.String(), tt.body)
			}
		})
	}
}

// Guards against a page on another site using the API through the user's browser.
func TestWebSocketOrigin(t *testing.T) {
	srv := httptest.NewServer(New(fakeDB{}))
	defer srv.Close()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"

	tests := []struct {
		name   string
		host   string
		origin string
		ok     bool
	}{
		{"same origin", "", srv.URL, true},
		{"other website", "", "https://evil.example", false},
		{"dns rebinding", "attacker.example:8080", "http://attacker.example:8080", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{Host: tt.host, HTTPHeader: http.Header{"Origin": {tt.origin}}})
			if !tt.ok {
				if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
					t.Fatalf("want 403, got err=%v resp=%v", err, resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow()
			var msg map[string]string
			if err := wsjson.Read(ctx, conn, &msg); err != nil || msg["type"] != "hello" {
				t.Fatalf("first message = %v, err = %v", msg, err)
			}
		})
	}
}
