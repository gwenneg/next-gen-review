package review

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu       sync.Mutex
	queued   []Review
	canceled bool // the user canceled while the review was being prepared
	prompt   string
	finished map[int64]Outcome
}

func (f *fakeStore) ClaimReview(context.Context) (Review, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.queued) == 0 {
		return Review{}, false, nil
	}
	r := f.queued[0]
	f.queued = f.queued[1:]
	return r, true, nil
}
func (f *fakeStore) CancelRequested(context.Context, int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canceled, nil
}
func (f *fakeStore) cancel() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.canceled = true
}
func (f *fakeStore) StartReview(_ context.Context, _ int64, prompt string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompt = prompt
	return !f.canceled, nil
}
func (f *fakeStore) ReviewProgress(context.Context, int64, int) (bool, error) { return false, nil }
func (f *fakeStore) FinishReview(ctx context.Context, id int64, o Outcome) error {
	if ctx.Err() != nil { // like pgx
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finished[id] = o
	return nil
}
func (f *fakeStore) outcome(id int64) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished[id]
}

type fakeGitHub struct {
	err   error
	delay time.Duration // a slow download
}

func (f fakeGitHub) PullRequest(_ context.Context, repo string, number int, sha string) (PR, error) {
	return PR{Repo: repo, Number: number, HeadSHA: sha, Title: "Add retries", Diff: "diff"}, f.err
}
func (f fakeGitHub) Tarball(context.Context, string, string) (io.ReadCloser, error) {
	time.Sleep(f.delay)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "o-r-abc/main.go", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("go"))
	_ = tw.Close()
	_ = gz.Close()
	return io.NopCloser(&buf), nil
}

var queued = Review{ID: 1, Repo: "o/r", Number: 7, HeadSHA: "seen", Model: "sonnet", Effort: "high", TokenCap: 1000, SessionID: "s"}

// startBroker runs a broker on one queued review until the test ends. jobTimeout 0 keeps the default.
func startBroker(t *testing.T, st *fakeStore, gh fakeGitHub, jobTimeout time.Duration) *Broker {
	st.queued, st.finished = []Review{queued}, map[int64]Outcome{}
	b := NewBroker(st, gh, t.TempDir())
	if jobTimeout != 0 {
		b.jobTimeout = jobTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return b
}

func TestBroker(t *testing.T) {
	tests := []struct {
		name     string
		gh       fakeGitHub
		canceled bool
		job      bool   // a job is returned
		finished string // the outcome stored for the review, if any
	}{
		{"prepared", fakeGitHub{}, false, true, ""},
		{"GitHub fails", fakeGitHub{err: errors.New("HTTP 502")}, false, false, StatusFailed},
		{"canceled while preparing", fakeGitHub{}, true, false, StatusCanceled},
		{"canceled, then GitHub fails", fakeGitHub{err: errors.New("HTTP 502")}, true, false, StatusCanceled},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{canceled: tt.canceled}
			b := startBroker(t, st, tt.gh, 0)
			job, ok, err := b.Next(t.Context(), time.Second)
			if err != nil || ok != tt.job {
				t.Fatalf("Next() = %v, %v, want a job: %v", ok, err, tt.job)
			}
			if got := st.outcome(1).Status; got != tt.finished {
				t.Errorf("stored outcome = %q, want %q", got, tt.finished)
			}
			if !tt.job {
				if _, err := os.Stat(filepath.Join(b.checkouts, "1")); err == nil {
					t.Error("the checkout of a review that didn't start was kept")
				}
				return
			}
			if job.Model != "sonnet" || job.Effort != "high" || job.TokenCap != 1000 || job.SessionID != "s" {
				t.Errorf("job = %+v, want the review's options", job)
			}
			if b, err := os.ReadFile(filepath.Join(job.Dir, "main.go")); err != nil || string(b) != "go" {
				t.Errorf("checkout main.go = %q, %v", b, err)
			}
			// The review covers the commit the user saw in the queue.
			if !strings.Contains(st.prompt, "head commit seen") || job.Prompt != st.prompt {
				t.Errorf("prompt doesn't name the queued commit:\n%s", st.prompt)
			}
			// The worker's request can end before the outcome is stored.
			ended, cancel := context.WithCancel(t.Context())
			cancel()
			b.Finish(ended, job.ID, Outcome{Status: StatusDone})
			if st.outcome(1).Status != StatusDone {
				t.Error("Finish didn't store the outcome once the request ended")
			}
			if _, err := os.Stat(job.Dir); err == nil {
				t.Error("Finish kept the checkout")
			}
		})
	}
}

// A download that outlasts the worker's request isn't lost: the next request gets the review.
func TestBrokerSlowPrepare(t *testing.T) {
	st := &fakeStore{}
	b := startBroker(t, st, fakeGitHub{delay: 300 * time.Millisecond}, 0)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, ok, _ := b.Next(ctx, time.Second); ok {
		t.Fatal("Next() returned a job before it was prepared")
	}
	time.Sleep(400 * time.Millisecond) // the worker asks again only after the download ended
	if _, ok, err := b.Next(t.Context(), time.Second); !ok || err != nil {
		t.Fatalf("Next() = %v, %v, want the review once prepared", ok, err)
	}
	if o := st.outcome(1); o.Status != "" {
		t.Errorf("the review was finished: %+v", o)
	}
}

// A prepared review the user cancels while the worker is busy or missing is canceled at once.
func TestBrokerCancelWhileWaiting(t *testing.T) {
	st := &fakeStore{}
	b := startBroker(t, st, fakeGitHub{}, 0)
	time.Sleep(100 * time.Millisecond) // prepared, and no worker asks for it
	st.cancel()
	b.Wake() // as the cancel handler does
	deadline := time.Now().Add(time.Second)
	for st.outcome(1).Status == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o := st.outcome(1); o.Status != StatusCanceled {
		t.Errorf("outcome = %+v, want canceled", o)
	}
	if _, err := os.Stat(filepath.Join(b.checkouts, "1")); err == nil {
		t.Error("the checkout of the canceled review was kept")
	}
}

// The worker's long poll ends when the api stops, so the shutdown doesn't wait for it.
func TestBrokerNextEndsOnStop(t *testing.T) {
	b := NewBroker(&fakeStore{finished: map[int64]Outcome{}}, fakeGitHub{}, t.TempDir())
	ctx, stop := context.WithCancel(t.Context())
	go b.Run(ctx)
	time.AfterFunc(100*time.Millisecond, stop)
	start := time.Now()
	if _, ok, err := b.Next(t.Context(), time.Minute); ok || err != nil {
		t.Fatalf("Next() = %v, %v, want no job", ok, err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("Next() returned %v after Run stopped", d)
	}
}

// A running review the worker stops reporting on is failed, so it doesn't block its PR forever.
func TestBrokerLostJob(t *testing.T) {
	st := &fakeStore{}
	b := startBroker(t, st, fakeGitHub{}, 100*time.Millisecond)
	if _, ok, err := b.Next(t.Context(), time.Second); !ok || err != nil {
		t.Fatalf("Next() = %v, %v", ok, err)
	}
	for range 10 { // reports keep it alive
		time.Sleep(30 * time.Millisecond)
		_, _ = b.Progress(t.Context(), 1, 0)
	}
	if o := st.outcome(1); o.Status != "" {
		t.Fatalf("a review the worker reports on was finished: %+v", o)
	}
	deadline := time.Now().Add(2 * time.Second)
	for st.outcome(1).Status == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if o := st.outcome(1); o.Status != StatusFailed || o.Error != "the worker stopped responding" {
		t.Errorf("outcome = %+v, want failed because the worker stopped responding", o)
	}
}

// Interrupted reviews leave checkouts behind; the api removes them at startup.
func TestRemoveCheckouts(t *testing.T) {
	b := NewBroker(&fakeStore{}, fakeGitHub{}, t.TempDir())
	if err := os.MkdirAll(filepath.Join(b.checkouts, "3", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveCheckouts(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(b.checkouts); len(entries) != 0 {
		t.Errorf("checkouts left: %v", entries)
	}
}

// The page warns that queued reviews won't start when the worker is missing.
func TestWorkerConnected(t *testing.T) {
	b := NewBroker(&fakeStore{finished: map[int64]Outcome{}}, fakeGitHub{}, t.TempDir())
	silent := func() {
		b.mu.Lock()
		b.lastCall = time.Now().Add(-workerTimeout)
		b.mu.Unlock()
	}
	if !b.WorkerConnected() {
		t.Error("not connected right after the api started, want a grace period")
	}
	silent()
	if b.WorkerConnected() {
		t.Error("connected after a silence of workerTimeout")
	}

	// During a long poll.
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _, _, _ = b.Next(ctx, time.Minute); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for !b.WorkerConnected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !b.WorkerConnected() {
		t.Error("not connected during a long poll")
	}
	cancel()
	<-done

	// A progress report during a review.
	silent()
	_, _ = b.Progress(t.Context(), 1, 0)
	if !b.WorkerConnected() {
		t.Error("not connected after a progress report")
	}
}
