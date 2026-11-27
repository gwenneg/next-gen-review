package events

import (
	"context"
	"testing"
	"time"
)

// After every (re)connect, subscribers are told to reload: events sent while the listener was
// disconnected are lost.
func TestBacklogSendsResync(t *testing.T) {
	h := NewHub()
	sub, unsubscribe := h.Subscribe()
	defer unsubscribe()
	if err := (handler{h}).HandleBacklog(context.Background(), "events", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-sub:
		if e.Type != EventResync {
			t.Errorf("event = %+v, want %s", e, EventResync)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event after the backlog handler ran")
	}
}
