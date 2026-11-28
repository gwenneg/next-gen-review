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
	"reflect"
	"slices"
	"strconv"
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
	failures int // FinishReview calls to fail, as when the database is down
	calls    int // FinishReview calls
	running  []int64
	reviews  map[int64]Review // for Review
	lines    DiffLines        // from StartReview
}

func (f *fakeStore) RunningReviews(context.Context) ([]int64, error) { return f.running, nil }
func (f *fakeStore) Review(_ context.Context, id int64) (Review, bool, error) {
	r, ok := f.reviews[id]
	return r, ok, nil
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
func (f *fakeStore) StartReview(_ context.Context, _ int64, prompt string, lines DiffLines) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prompt, f.lines = prompt, lines
	return !f.canceled, nil
}
func (f *fakeStore) ReviewProgress(context.Context, int64, int) (bool, error) { return false, nil }
func (f *fakeStore) FinishReview(ctx context.Context, id int64, o Outcome) error {
	if ctx.Err() != nil { // like pgx
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failures > 0 {
		f.failures--
		return errors.New("connection lost")
	}
	if _, ok := f.finished[id]; ok {
		return ErrNotActive
	}
	f.finished[id] = o
	return nil
}
func (f *fakeStore) outcome(id int64) Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.finished[id]
}

type fakeGitHub struct {
	err    error
	delay  time.Duration // a slow download
	cutOff bool          // the download stops halfway through a file
}

func (f fakeGitHub) PullRequest(_ context.Context, repo string, number int, sha string) (PR, error) {
	return PR{Repo: repo, Number: number, HeadSHA: sha, Title: "Add retries", Diff: "diff",
		Lines: DiffLines{"main.go": {SideRight: {{1, 2}}}}}, f.err
}
func (f fakeGitHub) Tarball(context.Context, string, string) (io.ReadCloser, error) {
	time.Sleep(f.delay)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "o-r-abc/main.go", Typeflag: tar.TypeReg, Mode: 0o644, Size: 2})
	_, _ = tw.Write([]byte("go"))
	if f.cutOff { // big.bin is created, then its data stops
		_ = tw.WriteHeader(&tar.Header{Name: "o-r-abc/big.bin", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1 << 20})
		_, _ = tw.Write(make([]byte, 100))
		_ = gz.Close()
		return io.NopCloser(&buf), nil
	}
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
		{"download cut off", fakeGitHub{cutOff: true}, false, false, StatusFailed},
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
			// The diff's lines are stored for the review's comments.
			if !reflect.DeepEqual(st.lines, DiffLines{"main.go": {SideRight: {{1, 2}}}}) {
				t.Errorf("stored lines = %v, want the PR's", st.lines)
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
	// Finish stores the outcome, then deletes the checkout: wait for both.
	kept := func() bool { _, err := os.Stat(filepath.Join(b.checkouts, "1")); return err == nil }
	deadline := time.Now().Add(time.Second)
	for (st.outcome(1).Status == "" || kept()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o := st.outcome(1); o.Status != StatusCanceled {
		t.Errorf("outcome = %+v, want canceled", o)
	}
	if kept() {
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

// An outcome the database refused is stored once it's back, instead of the review staying
// running forever. One for a review that already ended isn't retried.
func TestBrokerFinishRetries(t *testing.T) {
	st := &fakeStore{}
	b := startBroker(t, st, fakeGitHub{}, 100*time.Millisecond)
	if _, ok, err := b.Next(t.Context(), time.Second); !ok || err != nil {
		t.Fatalf("Next() = %v, %v", ok, err)
	}
	st.mu.Lock()
	st.failures = 2
	st.mu.Unlock()
	if err := b.Finish(t.Context(), 1, Outcome{Status: StatusDone, Result: "ok"}); err == nil {
		t.Error("Finish() = nil, want the error: the worker retries too")
	}
	deadline := time.Now().Add(2 * time.Second)
	for st.outcome(1).Status == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o := st.outcome(1); o.Status != StatusDone || o.Result != "ok" {
		t.Fatalf("outcome = %+v, want the worker's", o)
	}

	if err := b.Finish(t.Context(), 1, Outcome{Status: StatusFailed}); err != nil {
		t.Errorf("Finish() of an ended review = %v, want nil: there's nothing to retry", err)
	}
	st.mu.Lock()
	calls := st.calls
	st.mu.Unlock()
	time.Sleep(200 * time.Millisecond) // several retry rounds
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.calls != calls || st.finished[1].Status != StatusDone {
		t.Errorf("an ended review's outcome was retried: %d calls after %d, outcome %+v", st.calls, calls, st.finished[1])
	}
}

// An outcome the database keeps refusing is replaced by a plain failure, so the review ends.
func TestBrokerGivesUpOnRefusedOutcome(t *testing.T) {
	st := &fakeStore{}
	b := startBroker(t, st, fakeGitHub{}, 100*time.Millisecond)
	b.maxTries = 3
	if _, ok, err := b.Next(t.Context(), time.Second); !ok || err != nil {
		t.Fatalf("Next() = %v, %v", ok, err)
	}
	st.mu.Lock()
	st.failures = 3
	st.mu.Unlock()
	_ = b.Finish(t.Context(), 1, Outcome{Status: StatusDone, Result: "refused", Tokens: 40})
	deadline := time.Now().Add(2 * time.Second)
	for st.outcome(1).Status == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if o := st.outcome(1); o != (Outcome{Status: StatusFailed, Error: "the database refused the review's outcome", Tokens: 40}) {
		t.Errorf("outcome = %+v, want a plain failure with the tokens", o)
	}
}

// After an api restart, the reviews that were running are the worker's: their checkouts stay, and
// one the worker reports on goes on. The others leave checkouts behind, which are deleted.
func TestAdopt(t *testing.T) {
	st := &fakeStore{running: []int64{5, 6}, finished: map[int64]Outcome{}}
	b := NewBroker(st, fakeGitHub{}, t.TempDir())
	b.jobTimeout = 100 * time.Millisecond
	for _, dir := range []string{"3", "5", "6", "junk"} {
		if err := os.MkdirAll(filepath.Join(b.checkouts, dir, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Adopt(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := b.RemoveCheckouts(); err != nil {
		t.Fatal(err)
	}
	var left []string
	entries, _ := os.ReadDir(b.checkouts)
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if !slices.Equal(left, []string{"5", "6"}) {
		t.Errorf("checkouts left = %v, want the running reviews'", left)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go b.Run(ctx)
	for range 10 { // the worker reports on 5, not on 6
		time.Sleep(30 * time.Millisecond)
		_, _ = b.Progress(t.Context(), 5, 10)
	}
	if o := st.outcome(6); o.Error != "the worker stopped responding" {
		t.Errorf("review 6 = %+v, want failed: the worker doesn't run it", o)
	}
	if o := st.outcome(5); o.Status != "" {
		t.Errorf("review 5 = %+v, want it still running", o)
	}
	if err := b.Finish(t.Context(), 5, Outcome{Status: StatusDone}); err != nil || st.outcome(5).Status != StatusDone {
		t.Errorf("Finish(5) = %v, outcome %+v; want done", err, st.outcome(5))
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

// An ended review's code is downloaded again for `worker continue`, and deleted after. The code of
// an active one is its run's.
func TestPrepareContinue(t *testing.T) {
	ended := Review{ID: 4, Repo: "o/r", HeadSHA: "seen", Model: "haiku", Effort: "low", SessionID: "s4", Status: StatusDone}
	running := ended
	running.ID, running.Status = 5, StatusRunning
	tests := []struct {
		name string
		id   int64
		gh   fakeGitHub
		err  error // nil for a download
	}{
		{"ended", 4, fakeGitHub{}, nil},
		{"running", 5, fakeGitHub{}, ErrActive},
		{"unknown", 6, fakeGitHub{}, ErrUnknownReview},
		{"download cut off", 4, fakeGitHub{cutOff: true}, ErrDownload},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{reviews: map[int64]Review{4: ended, 5: running}}
			b := NewBroker(st, tt.gh, t.TempDir())
			// A checkout left by an earlier continue, or the running review's own.
			if err := os.MkdirAll(filepath.Join(b.checkouts, strconv.FormatInt(tt.id, 10), "old"), 0o755); err != nil {
				t.Fatal(err)
			}
			got, err := b.PrepareContinue(t.Context(), tt.id)
			_, oldErr := os.Stat(filepath.Join(b.checkouts, strconv.FormatInt(tt.id, 10), "old"))
			switch {
			case tt.err == nil:
				want := Resume{SessionID: "s4", Model: "haiku", Effort: "low", Dir: filepath.Join(b.checkouts, "4"), Download: got.Download}
				if err != nil || got != want || got.Download == "" {
					t.Fatalf("PrepareContinue() = %+v, %v; want %+v", got, err, want)
				}
				if body, err := os.ReadFile(filepath.Join(got.Dir, "main.go")); err != nil || string(body) != "go" || oldErr == nil {
					t.Errorf("checkout: main.go = %q, %v; old files kept: %v", body, err, oldErr == nil)
				}
				// A session continued again replaces the download; the first one's cleanup keeps it.
				again, err := b.PrepareContinue(t.Context(), 4)
				if err != nil || again.Download == got.Download {
					t.Fatalf("PrepareContinue() again = %+v, %v; want a new download", again, err)
				}
				if err := b.RemoveCheckout(t.Context(), 4, got.Download); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(got.Dir); err != nil {
					t.Error("an earlier session's cleanup deleted the newer download")
				}
				if err := b.RemoveCheckout(t.Context(), 4, again.Download); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(got.Dir); err == nil {
					t.Error("RemoveCheckout() kept the checkout")
				}
			case tt.err == ErrDownload:
				if !errors.Is(err, ErrDownload) {
					t.Fatalf("PrepareContinue() = %v, want a download error", err)
				}
				if entries, _ := os.ReadDir(b.checkouts); len(entries) != 0 {
					t.Errorf("a broken download left %v", entries)
				}
			default:
				if !errors.Is(err, tt.err) {
					t.Fatalf("PrepareContinue() = %v, want %v", err, tt.err)
				}
				if rmErr := b.RemoveCheckout(t.Context(), tt.id, ""); !errors.Is(rmErr, tt.err) || (tt.id == 5 && oldErr != nil) {
					t.Errorf("RemoveCheckout() = %v, want %v and the running review's checkout kept", rmErr, tt.err)
				}
			}
		})
	}
}

// The code of a continued session survives what would otherwise delete it: a late outcome from the
// review's own run, and an api restart. A second continue waits for the first download.
func TestContinuedCheckout(t *testing.T) {
	ended := Review{ID: 4, Repo: "o/r", HeadSHA: "seen", Model: "haiku", Effort: "low", SessionID: "s4", Status: StatusDone}
	st := &fakeStore{reviews: map[int64]Review{4: ended}, finished: map[int64]Outcome{4: {Status: StatusFailed}}}
	dir := t.TempDir()
	b := NewBroker(st, fakeGitHub{delay: 200 * time.Millisecond}, dir)
	if s, err := b.Session(t.Context(), 4); err != nil || s != (Resume{SessionID: "s4", Model: "haiku", Effort: "low"}) {
		t.Errorf("Session() = %+v, %v", s, err)
	}

	second := make(chan error, 1)
	go func() {
		time.Sleep(50 * time.Millisecond) // during the first download
		_, err := b.PrepareContinue(t.Context(), 4)
		second <- err
	}()
	got, err := b.PrepareContinue(t.Context(), 4)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, ErrDownloading) {
		t.Errorf("a second PrepareContinue() during the download = %v, want ErrDownloading", err)
	}

	// The review's run reports late: its outcome is dropped, and the code stays.
	if err := b.Finish(t.Context(), 4, Outcome{Status: StatusDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(got.Dir); err != nil {
		t.Fatal("a late Finish deleted the continued session's code")
	}

	// The api restarts: the code stays, and the worker's cleanup still deletes it after.
	b = NewBroker(st, fakeGitHub{}, dir)
	if err := os.MkdirAll(filepath.Join(dir, "8"), 0o755); err != nil { // a review interrupted while prepared
		t.Fatal(err)
	}
	if err := b.RemoveCheckouts(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "8")); err == nil {
		t.Error("RemoveCheckouts() kept an interrupted review's checkout")
	}
	if _, err := os.Stat(got.Dir); err != nil {
		t.Fatal("an api restart deleted the continued session's code")
	}
	if err := b.RemoveCheckout(t.Context(), 4, got.Download); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left after the session = %v", entries)
	}

	// A session that ended unseen a day ago: its code goes at the next restart.
	if _, err := b.PrepareContinue(t.Context(), 4); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-continuedFor - time.Minute)
	if err := os.Chtimes(filepath.Join(dir, "4"+continuedSuffix), old, old); err != nil {
		t.Fatal(err)
	}
	if err := NewBroker(st, fakeGitHub{}, dir).RemoveCheckouts(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left after a restart a day later = %v", entries)
	}
}
