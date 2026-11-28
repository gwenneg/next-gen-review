package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
	"github.com/gwenneg/next-gen-review/backend/internal/server"
	"github.com/gwenneg/next-gen-review/backend/internal/worker"
)

// fakeBroker hands out the queued jobs, failing the first fails calls to Next, and refusing to
// store the first refuse outcomes.
type fakeBroker struct {
	mu       sync.Mutex
	fails    int
	refuse   int
	tries    int // Finish calls
	jobs     []review.Job
	progress []int
	finished map[int64]review.Outcome
	done     chan struct{} // closed when the last job is finished
}

func (f *fakeBroker) Wake() {}
func (f *fakeBroker) Next(ctx context.Context, wait time.Duration) (review.Job, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fails > 0 {
		f.fails--
		return review.Job{}, false, errors.New("database down")
	}
	if len(f.jobs) == 0 {
		return review.Job{}, false, nil
	}
	job := f.jobs[0]
	f.jobs = f.jobs[1:]
	return job, true, nil
}
func (f *fakeBroker) Progress(_ context.Context, _ int64, tokens int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.progress = append(f.progress, tokens)
	return false, nil
}
func (f *fakeBroker) Finish(_ context.Context, id int64, o review.Outcome) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tries++
	if f.refuse > 0 {
		f.refuse--
		return errors.New("database down")
	}
	f.finished[id] = o
	if len(f.jobs) == 0 {
		close(f.done)
	}
	return nil
}
func (f *fakeBroker) WorkerConnected() bool { return true }

func TestServe(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = 10 * time.Millisecond
	done := review.Outcome{Status: review.StatusDone, Result: "## Findings", Tokens: 42}
	tests := []struct {
		name     string
		token    bool
		shutdown bool // the worker is stopped during the review
		refuse   int  // outcomes the api can't store, as while its database is down
		want     review.Outcome
		tries    int // reports of the outcome
	}{
		{"review", true, false, 0, done, 1},
		{"no token", false, false, 0, review.Outcome{Status: review.StatusFailed, Error: noToken}, 1},
		// The review's context is canceled, but the outcome is still reported.
		{"shut down during the review", true, true, 0, done, 1},
		{"the api can't store the outcome at first", true, false, 2, done, 3},
		// The worker stops: it doesn't wait for the api.
		{"shut down, and the api can't store the outcome", true, true, 1, review.Outcome{}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &fakeBroker{fails: 1, refuse: tt.refuse, jobs: []review.Job{{ID: 7}}, finished: map[int64]review.Outcome{}, done: make(chan struct{})}
			srv := httptest.NewServer(server.NewInternal(b))
			defer srv.Close()
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			run := func(ctx context.Context, job review.Job, report worker.Report) review.Outcome {
				if _, err := report(ctx, 42); err != nil {
					t.Errorf("report: %v", err)
				}
				if tt.shutdown {
					stop()
				}
				return done
			}
			served := make(chan struct{})
			go func() { serve(ctx, worker.NewAPI(srv.URL), run, tt.token); close(served) }()
			select {
			case <-b.done:
			case <-served:
			case <-time.After(5 * time.Second):
				t.Fatal("the job wasn't finished: the worker gave up after the api's error?")
			}
			stop()
			<-served
			b.mu.Lock()
			defer b.mu.Unlock()
			if got := b.finished[7]; got != tt.want || b.tries != tt.tries {
				t.Errorf("outcome = %+v after %d reports, want %+v after %d", got, b.tries, tt.want, tt.tries)
			}
			if tt.token && len(b.progress) != 1 {
				t.Errorf("progress reports = %v, want the run's", b.progress)
			}
		})
	}
}

// `worker check` reports what claude answered, or why it couldn't run.
func TestCheck(t *testing.T) {
	tests := []struct {
		name, token, out string
		exit             int
		err              string // what the error says, empty for none
	}{
		{"no token", "", "", 0, "CLAUDE_CODE_OAUTH_TOKEN is not set"},
		{"passes", "fake-token", `{"is_error":false,"result":"OK","duration_ms":800,"modelUsage":{"claude-haiku":{"inputTokens":5,"outputTokens":1}}}`, 0, ""},
		{"claude refuses the token", "fake-token", `{"is_error":true,"result":"Invalid bearer token"}`, 0, "Invalid bearer token"},
		{"not JSON", "fake-token", `oops`, 0, "parse claude output"},
		{"claude fails", "fake-token", ``, 1, "run claude"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\ncat <<'EOF'\n%s\nEOF\nexit %d\n", tt.out, tt.exit)
			if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", tt.token)
			err := check()
			if (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Errorf("check() = %v, want an error saying %q", err, tt.err)
			}
		})
	}
}

// An outcome the api refuses as a bad request would be refused again: the worker gives up.
func TestFinishGivesUpOnRefusal(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = 10 * time.Millisecond
	var tries atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		tries.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()
	finish(t.Context(), worker.NewAPI(srv.URL), 7, review.Outcome{Status: review.StatusDone})
	if n := tries.Load(); n != 1 {
		t.Errorf("tries = %d, want 1", n)
	}
}
