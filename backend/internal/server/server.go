// Package server serves the HTTP API and the WebSocket used for live updates.
package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// Pinger is the part of the database pool the health check needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// New returns the API handler.
func New(db Pinger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", health(db))
	mux.HandleFunc("GET /api/ws", events)
	return localOnly(mux)
}

// localOnly rejects requests whose Host isn't a loopback name. With DNS rebinding, a page on
// another site makes its own hostname resolve to 127.0.0.1, so its requests reach the api with
// that hostname in Host. Browsers set Host themselves, so a page can't fake a loopback one.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch host {
		case "localhost", "127.0.0.1", "::1", "[::1]":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "host not allowed", http.StatusForbidden)
		}
	})
}

func health(db Pinger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()

		status, body := http.StatusOK, map[string]string{"status": "ok", "database": "ok"}
		if err := db.Ping(ctx); err != nil {
			slog.Warn("health check: database unreachable", "error", err)
			status, body = http.StatusServiceUnavailable, map[string]string{"status": "degraded", "database": "unreachable"}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}
}

// events upgrades to a WebSocket. For now it only sends a hello message, then keeps the connection open.
func events(w http.ResponseWriter, r *http.Request) {
	// Accept rejects browser requests whose Origin doesn't match the Host header, so another
	// website open in the browser can't connect. localOnly covers DNS rebinding, where both
	// match. Don't set InsecureSkipVerify.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	ctx := conn.CloseRead(r.Context())
	if err := wsjson.Write(ctx, conn, map[string]string{"type": "hello"}); err != nil {
		return
	}
	<-ctx.Done()
}
