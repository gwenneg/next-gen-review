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
	api := worker.NewAPI(base)
	claude := worker.Claude{Bin: "claude", Env: worker.Env(), Timeout: 30 * time.Minute, ProgressEvery: 2 * time.Second}
	slog.Info("worker started")
	for ctx.Err() == nil {
		job, ok, err := api.Next(ctx)
		if err != nil {
			if ctx.Err() == nil {
				slog.Warn("get the next job", "error", err)
				sleep(ctx, 5*time.Second)
			}
			continue
		}
		if !ok {
			continue
		}
		slog.Info("review started", "review", job.ID, "model", job.Model, "effort", job.Effort)
		outcome := review.Outcome{Status: review.StatusFailed, Error: noToken}
		if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
			outcome = claude.Run(ctx, job, func(ctx context.Context, tokens int) (bool, error) {
				return api.Progress(ctx, job.ID, tokens)
			})
		}
		slog.Info("review finished", "review", job.ID, "status", outcome.Status, "tokens", outcome.Tokens)
		// The review's own context may be canceled: report with a fresh one.
		finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		if err := api.Finish(finishCtx, job.ID, outcome); err != nil {
			slog.Warn("report the review's end", "review", job.ID, "error", err)
		}
		cancel()
	}
	slog.Info("worker stopped")
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
