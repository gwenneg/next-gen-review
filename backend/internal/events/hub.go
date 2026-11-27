// Package events forwards the events Postgres announces with NOTIFY to the WebSocket clients.
package events

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgxlisten"
)

// EventResync tells clients to reload everything: events sent while the listener was
// disconnected are lost.
const EventResync = "resync"

// Event is what a client receives. Clients reload the data the type names.
type Event struct {
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
}

// Hub fans events out to subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}}
}

// Subscribe returns a channel of events and a function to unsubscribe. The hub closes the
// channel if the subscriber falls behind, so the client reconnects and reloads its data.
func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}

func (h *Hub) publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// Listen runs LISTEN on its own connection (standard PG* environment variables) until ctx ends.
// pgxlisten reconnects after errors.
func (h *Hub) Listen(ctx context.Context) {
	l := &pgxlisten.Listener{
		Connect:        func(ctx context.Context) (*pgx.Conn, error) { return pgx.Connect(ctx, "") },
		LogError:       func(_ context.Context, err error) { slog.Warn("event listener", "error", err) },
		ReconnectDelay: 2 * time.Second,
	}
	l.Handle("events", handler{h})
	if err := l.Listen(ctx); err != nil && ctx.Err() == nil {
		slog.Error("event listener stopped", "error", err)
	}
}

// handler implements pgxlisten.Handler and pgxlisten.BacklogHandler.
type handler struct{ h *Hub }

func (x handler) HandleNotification(_ context.Context, n *pgconn.Notification, _ *pgx.Conn) error {
	var e Event
	if err := json.Unmarshal([]byte(n.Payload), &e); err != nil {
		return fmt.Errorf("malformed event %q: %w", n.Payload, err)
	}
	x.h.publish(e)
	return nil
}

// HandleBacklog runs after every (re)connect, once LISTEN is active again.
func (x handler) HandleBacklog(context.Context, string, *pgx.Conn) error {
	x.h.publish(Event{Type: EventResync})
	return nil
}
