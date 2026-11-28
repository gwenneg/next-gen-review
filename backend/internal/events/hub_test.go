package events

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gwenneg/next-gen-review/backend/internal/dbtest"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(dbtest.Main(m, &pool)) }

// next returns the next event, or fails after 5s.
func next(t *testing.T, sub <-chan Event) Event {
	t.Helper()
	select {
	case e := <-sub:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
		return Event{}
	}
}

// Listen forwards the NOTIFY events to subscribers, and goes on after a malformed one.
func TestListen(t *testing.T) {
	h := NewHub()
	sub, unsubscribe := h.Subscribe()
	defer unsubscribe()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { h.Listen(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	if e := next(t, sub); e.Type != EventResync {
		t.Fatalf("first event = %+v, want %s once listening", e, EventResync)
	}
	for _, payload := range []string{`not json`, `{"seq": 4, "type": "queue.changed"}`} {
		if _, err := pool.Exec(t.Context(), `SELECT pg_notify('events', $1)`, payload); err != nil {
			t.Fatal(err)
		}
	}
	if e := next(t, sub); e != (Event{Seq: 4, Type: "queue.changed"}) {
		t.Errorf("event = %+v, want the second notification", e)
	}
}

// A subscriber that falls behind is dropped: its channel is closed, so the client reconnects and
// reloads instead of missing events silently.
func TestSlowSubscriberDropped(t *testing.T) {
	h := NewHub()
	sub, unsubscribe := h.Subscribe()
	defer unsubscribe() // safe after the hub dropped it
	for i := range 17 {
		h.publish(Event{Seq: int64(i), Type: "queue.changed"})
	}
	n := 0
	for range sub {
		n++
	}
	if n != 16 {
		t.Errorf("got %d events before the channel closed, want the 16 buffered", n)
	}
}
