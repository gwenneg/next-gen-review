// Command worker runs the AI reviews. It holds the Claude subscription token and nothing else: it
// gets its jobs and the code from the api, never from GitHub or the database.
//
// `worker check` makes one tiny claude -p call, to check the token works in the container.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
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
