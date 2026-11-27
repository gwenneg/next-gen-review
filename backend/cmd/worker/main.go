// Command worker will run the AI reviews. It holds the Claude token and never the GitHub token.
//
// For now it only waits, and `worker check` makes one tiny `claude -p` call to prove
// the subscription token works in the container.
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
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "check" {
		if err := check(); err != nil {
			fmt.Fprintln(os.Stderr, "claude check failed:", err)
			os.Exit(1)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("worker started, no job queue yet")
	<-ctx.Done()
	slog.Info("worker stopped")
}

// result holds the fields we use from `claude -p --output-format json`.
type result struct {
	IsError    bool   `json:"is_error"`
	Result     string `json:"result"`
	DurationMS int    `json:"duration_ms"`
	ModelUsage map[string]struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
	} `json:"modelUsage"`
}

func check() error {
	if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return errors.New("CLAUDE_CODE_OAUTH_TOKEN is not set: run `claude setup-token` on your laptop and put the token in .env")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// No tools, no MCP servers, nothing saved: the call only proves that auth works.
	cmd := exec.CommandContext(ctx, "claude", "-p",
		"--model", "haiku",
		"--output-format", "json",
		"--tools", "",
		"--strict-mcp-config",
		"--no-session-persistence",
		"Reply with the single word OK.")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("run claude: %w\n%s", err, out)
	}

	var r result
	if err := json.Unmarshal(out, &r); err != nil {
		return fmt.Errorf("parse claude output: %w\n%s", err, out)
	}
	if r.IsError {
		return fmt.Errorf("claude returned an error: %s", r.Result)
	}
	fmt.Printf("claude check passed in %dms. Reply: %q\n", r.DurationMS, r.Result)
	for model, u := range r.ModelUsage {
		fmt.Printf("  %s: %d input tokens, %d output tokens\n", model, u.InputTokens, u.OutputTokens)
	}
	return nil
}
