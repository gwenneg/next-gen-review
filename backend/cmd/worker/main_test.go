package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
func (f *fakeBroker) Session(context.Context, int64) (review.Resume, error) {
	return review.Resume{}, review.ErrUnknownReview
}
func (f *fakeBroker) PrepareContinue(context.Context, int64) (review.Resume, error) {
	return review.Resume{}, review.ErrUnknownReview
}
func (f *fakeBroker) RemoveCheckout(context.Context, int64, string) error {
	return review.ErrUnknownReview
}

// continueBroker answers `worker continue` about review 7.
type continueBroker struct {
	fakeBroker
	resume    review.Resume
	err       error
	removed   []string // the downloads RemoveCheckout was called for
	downloads int      // PrepareContinue calls
}

func (c *continueBroker) Session(context.Context, int64) (review.Resume, error) {
	return review.Resume{SessionID: c.resume.SessionID, Model: c.resume.Model, Effort: c.resume.Effort}, c.err
}
func (c *continueBroker) PrepareContinue(context.Context, int64) (review.Resume, error) {
	c.downloads++
	return c.resume, c.err
}
func (c *continueBroker) RemoveCheckout(_ context.Context, _ int64, download string) error {
	c.removed = append(c.removed, download)
	return nil
}

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

// `worker continue` opens the review's session in a new download of its code, with the review's
// restricting flags, and has the code deleted after.
func TestContinue(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		token     string
		session   bool  // claude saved a session for the review
		apiErr    error // from the broker
		exit      int   // claude's exit code
		err       string
		claudeRun bool
	}{
		{"continue", []string{"7"}, "fake-token", true, nil, 0, "", true},
		{"claude fails", []string{"7"}, "fake-token", true, nil, 1, "claude: exit status 1", true},
		// It's found before downloading anything.
		{"no session", []string{"7"}, "fake-token", false, nil, 0, "no claude session in this worker", false},
		{"review still running", []string{"7"}, "fake-token", true, review.ErrActive, 0, "409 Conflict: the review is still active", false},
		{"no token", []string{"7"}, "", true, nil, 0, "CLAUDE_CODE_OAUTH_TOKEN is not set", false},
		{"no id", nil, "fake-token", true, nil, 0, "usage: worker continue <review id>", false},
		{"bad id", []string{"seven"}, "fake-token", true, nil, 0, `"seven" is not a review id`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp, checkout, config := t.TempDir(), t.TempDir(), t.TempDir()
			if tt.session {
				if err := os.MkdirAll(filepath.Join(config, "projects", "-checkouts-7"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(config, "projects", "-checkouts-7", "s7.jsonl"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			bin := filepath.Join(tmp, "claude")
			script := fmt.Sprintf("#!/bin/sh\necho \"$@\" > %s/args\npwd > %s/pwd\nexit %d\n", tmp, tmp, tt.exit)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", tt.token)
			b := &continueBroker{resume: review.Resume{SessionID: "s7", Model: "haiku", Effort: "low", Dir: checkout, Download: "d1"}, err: tt.apiErr}
			srv := httptest.NewServer(server.NewInternal(b))
			defer srv.Close()
			var out strings.Builder
			// A session of the review left running by a closed terminal.
			proc := t.TempDir()
			earlier := exec.Command("sleep", "30")
			if err := earlier.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = earlier.Process.Kill() }()
			stopped := make(chan struct{})
			go func() { _ = earlier.Wait(); close(stopped) }()
			pid := filepath.Join(proc, strconv.Itoa(earlier.Process.Pid))
			_ = os.MkdirAll(pid, 0o755)
			_ = os.WriteFile(filepath.Join(pid, "cmdline"), []byte("claude\x00--resume\x00s7\x00"), 0o644)
			c := session{api: worker.NewAPI(srv.URL), claude: bin, configDir: config, procDir: proc,
				stdin: strings.NewReader(""), stdout: &out, stderr: &out}

			err := c.continueReview(t.Context(), tt.args)
			if (tt.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("continueReview() = %v, want an error saying %q", err, tt.err)
			}
			args, runErr := os.ReadFile(filepath.Join(tmp, "args"))
			if ran := runErr == nil; ran != tt.claudeRun {
				t.Fatalf("claude ran: %v, want %v", ran, tt.claudeRun)
			}
			downloaded := tt.token != "" && len(tt.args) == 1 && tt.args[0] == "7" && tt.apiErr == nil && tt.session
			if downloaded != (b.downloads == 1) {
				t.Errorf("downloads = %d, want a download: %v", b.downloads, downloaded)
			}
			if downloaded != slices.Equal(b.removed, []string{"d1"}) {
				t.Errorf("checkout deletes = %v, want the download deleted once after it", b.removed)
			}
			select {
			case <-stopped:
				if !downloaded {
					t.Error("an earlier session was stopped though nothing was downloaded")
				}
			case <-time.After(100 * time.Millisecond):
				if downloaded {
					t.Error("the earlier session of the review is still running")
				}
			}
			if !tt.claudeRun {
				return
			}
			want := "--resume s7 --model haiku --effort low --tools Read,Grep,Glob --permission-mode dontAsk --restricted --safe-mode --strict-mcp-config"
			if got := strings.TrimSpace(string(args)); got != want {
				t.Errorf("claude args = %s\nwant %s", got, want)
			}
			wd, _ := os.ReadFile(filepath.Join(tmp, "pwd"))
			if real, _ := filepath.EvalSymlinks(checkout); strings.TrimSpace(string(wd)) != real {
				t.Errorf("claude ran in %s, want the checkout %s", wd, real)
			}
		})
	}
}

// A session left running when its terminal went away is stopped before the review is continued
// again. Other processes, and other reviews' sessions, are left alone.
func TestStopEarlier(t *testing.T) {
	proc := t.TempDir()
	start := func(cmdline ...string) *exec.Cmd {
		cmd := exec.Command("sleep", "30")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		dir := filepath.Join(proc, strconv.Itoa(cmd.Process.Pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(cmdline, "\x00")+"\x00"), 0o644); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	// ended is closed when the process exits: a killed child stays a zombie until waited for.
	ended := func(cmd *exec.Cmd) chan struct{} {
		c := make(chan struct{})
		go func() { _ = cmd.Wait(); close(c) }()
		return c
	}
	earlier := ended(start("claude", "--resume", "s7", "--model", "haiku"))
	others := []chan struct{}{ended(start("claude", "--resume", "s8")), ended(start("worker", "continue", "7"))}

	stopEarlier(proc, "s7", 5*time.Second)
	select {
	case <-earlier:
	case <-time.After(time.Second):
		t.Fatal("the earlier session is still running")
	}
	time.Sleep(100 * time.Millisecond)
	for i, other := range others {
		select {
		case <-other:
			t.Errorf("process %d was stopped", i)
		default:
		}
	}
}

// A session that ignores SIGTERM is killed after the wait.
func TestStopEarlierKills(t *testing.T) {
	proc := t.TempDir()
	cmd := exec.Command("sh", "-c", `trap "" TERM; exec sleep 30`)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	ended := make(chan struct{})
	go func() { _ = cmd.Wait(); close(ended) }()
	time.Sleep(100 * time.Millisecond) // the trap is set
	pid := filepath.Join(proc, strconv.Itoa(cmd.Process.Pid))
	_ = os.MkdirAll(pid, 0o755)
	_ = os.WriteFile(filepath.Join(pid, "cmdline"), []byte("claude\x00--resume\x00s7\x00"), 0o644)
	stopEarlier(proc, "s7", 200*time.Millisecond)
	select {
	case <-ended:
	case <-time.After(time.Second):
		t.Fatal("a session that ignores SIGTERM still runs")
	}
}

// A zombie has exited: its parent just hasn't waited for it yet, and signals still reach it.
func TestRunning(t *testing.T) {
	proc := t.TempDir()
	for pid, stat := range map[string]string{
		"10": "10 (claude) S 1 10 10 0 -1",     // sleeping
		"11": "11 (sh -c a) b) Z 1 11 11 0 -1", // exited, with a ")" in its command
	} {
		_ = os.MkdirAll(filepath.Join(proc, pid), 0o755)
		_ = os.WriteFile(filepath.Join(proc, pid, "stat"), []byte(stat), 0o644)
	}
	if !running(proc, 10) || running(proc, 11) {
		t.Errorf("running(10, 11) = %v, %v; want true, false", running(proc, 10), running(proc, 11))
	}
}
