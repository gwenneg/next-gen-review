// Package queue keeps the queue table in sync with GitHub.
package queue

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/store"
)

// Source fetches the queue from GitHub.
type Source interface {
	Queue(ctx context.Context, watched []string) ([]github.PullRequest, error)
}

// Store is the part of store.Store the poller uses.
type Store interface {
	WatchedRepos(ctx context.Context) ([]string, error)
	ReplaceQueue(ctx context.Context, queue []github.PullRequest) error
	AddEvent(ctx context.Context, eventType string) error
}

// Status is the result of the last poll.
type Status struct {
	RefreshedAt *time.Time `json:"refreshedAt,omitempty"` // last successful poll
	Error       string     `json:"error,omitempty"`       // last poll's error, if it failed
}

// Poller polls GitHub on a timer and on request.
type Poller struct {
	src      Source
	store    Store
	interval time.Duration
	wake     chan struct{}

	pollMu   sync.Mutex // one poll at a time
	lastHash [32]byte   // guarded by pollMu

	mu     sync.Mutex // guards status
	status Status
}

// NewPoller returns a poller. Call Run to start it.
func NewPoller(src Source, st Store, interval time.Duration) *Poller {
	return &Poller{src: src, store: st, interval: interval, wake: make(chan struct{}, 1)}
}

// Run polls until ctx ends.
func (p *Poller) Run(ctx context.Context) {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		_ = p.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-p.wake:
		}
	}
}

// Wake asks Run for a poll soon, without waiting for it.
func (p *Poller) Wake() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Status returns the result of the last poll.
func (p *Poller) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// Poll fetches the queue now and stores it if it changed. On error the stored queue stays as it
// was. The status is updated before the event is sent, so a page that reloads sees it.
func (p *Poller) Poll(ctx context.Context) error {
	p.pollMu.Lock()
	defer p.pollMu.Unlock()

	queue, err := p.fetch(ctx)
	if err != nil {
		slog.Warn("queue poll failed", "error", err)
		if p.setStatus(Status{Error: err.Error(), RefreshedAt: p.Status().RefreshedAt}) {
			_ = p.store.AddEvent(ctx, store.EventQueueChanged) // so the page shows the error
		}
		return err
	}

	now, before := time.Now(), p.Status()
	statusChanged := p.setStatus(Status{RefreshedAt: &now})
	body, _ := json.Marshal(queue)
	if hash := sha256.Sum256(body); hash != p.lastHash {
		if err := p.store.ReplaceQueue(ctx, queue); err != nil {
			failed := Status{Error: "save queue: " + err.Error(), RefreshedAt: before.RefreshedAt}
			p.setStatus(failed)
			if failed.Error != before.Error {
				_ = p.store.AddEvent(ctx, store.EventQueueChanged) // so the page shows the error
			}
			return err
		}
		p.lastHash = hash
	} else if statusChanged { // same queue, but the page must drop the old error
		_ = p.store.AddEvent(ctx, store.EventQueueChanged)
	}
	return nil
}

// setStatus stores the status and says whether its error changed.
func (p *Poller) setStatus(s Status) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := s.Error != p.status.Error
	p.status = s
	return changed
}

func (p *Poller) fetch(ctx context.Context) ([]github.PullRequest, error) {
	watched, err := p.store.WatchedRepos(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	return p.src.Queue(ctx, watched)
}
