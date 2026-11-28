// Command worker runs the AI reviews. It holds the Claude subscription token and nothing else: it
// gets its jobs and the code from the api, never from GitHub or the database.
//
// `worker check` makes one tiny claude -p call, to check the token works in the container.
//
// `worker continue <review id>` opens an ended review's claude session in this terminal, for
// follow-up questions. `make continue REVIEW=<id>` runs it in the worker container.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
	"github.com/gwenneg/next-gen-review/backend/internal/worker"
)

const noToken = "CLAUDE_CODE_OAUTH_TOKEN is not set: run `claude setup-token` on your laptop, put the token in .env, and restart the stack"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "check" {
		if err := check(); err != nil {
			fmt.Fprintln(os.Stderr, "claude check failed:", err)
			os.Exit(1)
		}
		return
	}

	base := os.Getenv("API_INTERNAL_URL")
	if base == "" {
		slog.Error("API_INTERNAL_URL is not set: start the worker with Compose, and recreate its container after changing compose.yaml")
		os.Exit(1)
	}
	if len(os.Args) > 1 && os.Args[1] == "continue" {
		c := session{api: worker.NewAPI(base), claude: "claude", configDir: os.Getenv("CLAUDE_CONFIG_DIR"), procDir: "/proc",
			stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr}
		if err := c.continueReview(context.Background(), os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "continue the review:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	claude := worker.Claude{Bin: "claude", Env: worker.Env(), Timeout: 30 * time.Minute, ProgressEvery: 2 * time.Second}
	slog.Info("worker started")
	serve(ctx, worker.NewAPI(base), claude.Run, os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "")
	slog.Info("worker stopped")
}

// retryDelay is how long the worker waits after a failed call to the api.
var retryDelay = 5 * time.Second

// serve runs the api's jobs one at a time, until ctx is done. Without a token, each job fails
// with a message that says how to set one.
func serve(ctx context.Context, api *worker.API, run func(context.Context, review.Job, worker.Report) review.Outcome, token bool) {
	for ctx.Err() == nil {
		job, ok, err := api.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("get the next job", "error", err)
				sleep(ctx, retryDelay)
			}
			continue
		}
		if !ok {
			continue
		}
		slog.Info("review started", "review", job.ID, "model", job.Model, "effort", job.Effort)
		outcome := review.Outcome{Status: review.StatusFailed, Error: noToken}
		if token {
			outcome = run(ctx, job, func(ctx context.Context, tokens int) (bool, error) {
				return api.Progress(ctx, job.ID, tokens)
			})
		}
		slog.Info("review finished", "review", job.ID, "status", outcome.Status, "tokens", outcome.Tokens)
		finish(ctx, api, job.ID, outcome)
	}
}

// finish reports the outcome, again every retryDelay until the api stored it: the api may be
// restarting, or its database down. It gives up when the api refuses the request, and after one
// try when the worker is stopping. Each try has its own context: the review's may be canceled.
func finish(ctx context.Context, api *worker.API, id int64, o review.Outcome) {
	for {
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		err := api.Finish(finishCtx, id, o)
		cancel()
		if err == nil {
			return
		}
		var refused *worker.StatusError
		if errors.As(err, &refused) && refused.Code < 500 || ctx.Err() != nil {
			slog.Error("report the review's end: giving up", "review", id, "error", err)
			return
		}
		slog.Warn("report the review's end: retrying", "review", id, "error", err)
		sleep(ctx, retryDelay)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func check() error {
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return errors.New(noToken)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// No tools, no MCP servers, nothing saved: the call only proves that auth works.
	cmd := exec.CommandContext(ctx, "claude", "-p", "--model", "haiku", "--output-format", "json",
		"--tools", "", "--strict-mcp-config", "--no-session-persistence", "Reply with the single word OK.")
	cmd.Env, cmd.Stderr = worker.Env(), os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("run claude: %w\n%s", err, out)
	}
	var r struct {
		IsError    bool   `json:"is_error"`
		Result     string `json:"result"`
		DurationMS int    `json:"duration_ms"`
		ModelUsage map[string]struct {
			InputTokens  int `json:"inputTokens"`
			OutputTokens int `json:"outputTokens"`
		} `json:"modelUsage"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return fmt.Errorf("parse claude output: %w\n%s", err, out)
	}
	if r.IsError {
		return errors.New("claude returned an error: " + r.Result)
	}
	fmt.Printf("claude check passed in %dms. Reply: %q\n", r.DurationMS, r.Result)
	for model, u := range r.ModelUsage {
		fmt.Printf("  %s: %d input tokens, %d output tokens\n", model, u.InputTokens, u.OutputTokens)
	}
	return nil
}

// session opens an ended review's claude session again, in a terminal.
type session struct {
	api            *worker.API
	claude         string // the claude binary
	configDir      string // claude's CLAUDE_CONFIG_DIR, which holds the sessions
	procDir        string // /proc, where stopEarlier looks for an earlier session
	stdin          io.Reader
	stdout, stderr io.Writer
}

// continueReview gets a new download of the review's code from the api, runs claude --resume in
// it with the review's flags, and has the api delete the code once claude exits.
func (c session) continueReview(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: worker continue <review id>, the number the page shows")
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return fmt.Errorf("%q is not a review id: use the number the page shows", args[0])
	}
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return errors.New(noToken)
	}
	s, err := c.api.Session(ctx, id)
	if err != nil {
		return err
	}
	if found, _ := filepath.Glob(filepath.Join(c.configDir, "projects", "*", s.SessionID+".jsonl")); len(found) == 0 {
		return fmt.Errorf("review %d has no claude session in this worker: claude never ran for it, claude deleted it "+
			"(it keeps sessions 30 days by default), or it ran under the other stack (make dev and make up keep separate sessions)", id)
	}
	fmt.Fprintf(c.stderr, "Downloading the code of review %d again…\n", id)
	r, err := c.api.Continue(ctx, id)
	if err != nil {
		return err
	}
	stopEarlier(c.procDir, r.SessionID, 5*time.Second)
	defer func() {
		removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if err := c.api.RemoveCheckout(removeCtx, id, r.Download); err != nil {
			fmt.Fprintln(c.stderr, "delete the code:", err)
		}
	}()
	cmd := exec.CommandContext(ctx, c.claude, append([]string{"--resume", r.SessionID}, worker.Flags(r.Model, r.Effort)...)...)
	cmd.Dir, cmd.Env = r.Dir, worker.Env()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.stdin, c.stdout, c.stderr
	// Ctrl-C is claude's: the worker must stay to delete the code after. A caught signal, unlike an
	// ignored one, is back to its default in claude.
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	return nil
}

// stopEarlier stops the claude processes still resuming this session: with SIGTERM, then SIGKILL
// for any still running after wait. When its terminal goes away, a `worker continue` keeps running
// in the container: Docker doesn't stop exec'd processes when the client disconnects. Two claude
// processes would write the same session.
func stopEarlier(procDir, sessionID string, wait time.Duration) {
	var pids []int
	entries, _ := os.ReadDir(procDir)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(procDir, e.Name(), "cmdline"))
		args := strings.Split(string(cmdline), "\x00")
		if i := slices.Index(args, "--resume"); err == nil && i >= 0 && i+1 < len(args) && args[i+1] == sessionID {
			pids = append(pids, pid)
		}
	}
	for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		deadline := time.Now().Add(wait)
		for _, pid := range pids {
			if !running(procDir, pid) {
				continue
			}
			_ = syscall.Kill(pid, sig)
			for running(procDir, pid) && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
		}
	}
}

// running says whether the process still runs: an exited one may stay a zombie, which signals
// still reach, until its parent waits for it.
func running(procDir string, pid int) bool {
	stat, err := os.ReadFile(filepath.Join(procDir, strconv.Itoa(pid), "stat"))
	if err != nil {
		return syscall.Kill(pid, 0) == nil
	}
	// The state comes after the command, which is in parentheses and may hold spaces.
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}
