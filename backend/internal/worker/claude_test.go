package worker

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// fakeClaude writes a script that saves its arguments to args, prints the given stream-json lines,
// then hangs if hang is set, as a long review would.
func fakeClaude(t *testing.T, hang bool, lines ...string) Claude {
	dir := t.TempDir()
	events := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(events, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "args") + "\ncat " + events + "\n"
	if hang {
		script += "exec sleep 30\n"
	}
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return Claude{Bin: bin, Env: []string{"PATH=" + os.Getenv("PATH")}, Timeout: 10 * time.Second,
		ProgressEvery: 50 * time.Millisecond}
}

const (
	initOK     = `{"type":"system","subtype":"init","apiKeySource":"none"}`
	initAPIKey = `{"type":"system","subtype":"init","apiKeySource":"ANTHROPIC_API_KEY"}`
	// m1 counts 100+50+10 = 160 tokens (cache reads are not counted). claude repeats a message's
	// usage for each of its content blocks, so m1 appears twice.
	m1a    = `{"type":"assistant","message":{"id":"m1","content":[{"type":"text","text":"Looking at the diff."}],"usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":9000,"output_tokens":10}}}`
	m1b    = `{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use"}],"usage":{"input_tokens":100,"cache_creation_input_tokens":50,"cache_read_input_tokens":9000,"output_tokens":10}}}`
	m2     = `{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"Done."}],"usage":{"input_tokens":10,"output_tokens":5}}}`
	result = `{"type":"result","subtype":"success","is_error":false,"result":"## Findings\nNone."}`
)

func TestRun(t *testing.T) {
	job := review.Job{ID: 1, Model: "sonnet", Effort: "medium", TokenCap: 1000, SessionID: "s", Prompt: "review"}
	never := func(context.Context, int) (bool, error) { return false, nil }
	timeout := fakeClaude(t, true, initOK)
	timeout.Timeout = time.Second
	defer func(n int) { maxLine = n }(maxLine)
	maxLine = 1 << 10
	long := `{"type":"user","content":"` + strings.Repeat("a", 2<<10) + `"}`
	tests := []struct {
		name     string
		claude   Claude
		cap      int
		report   Report
		shutdown bool // the worker is stopped during the run
		want     review.Outcome
	}{
		{"done", fakeClaude(t, false, initOK, m1a, m1b, m2, result), 1000, never, false,
			review.Outcome{Status: review.StatusDone, Result: "## Findings\nNone.", Tokens: 175}},
		// Events claude already sent before being stopped are still counted.
		{"API key instead of the subscription", fakeClaude(t, true, initAPIKey, m1a), 1000, never, false,
			review.Outcome{Status: review.StatusFailed, Result: "Looking at the diff.", Tokens: 160,
				Error: `stopped: claude was not using the subscription token (apiKeySource "ANTHROPIC_API_KEY")`}},
		{"token cap", fakeClaude(t, true, initOK, m1a), 100, never, false,
			review.Outcome{Status: review.StatusCapped, Result: "Looking at the diff.", Error: "stopped at the token cap of 100", Tokens: 160}},
		// Cancel is seen while claude prints nothing, as during a long thinking turn.
		{"canceled while claude is silent", fakeClaude(t, true, initOK, m1a), 1000, cancelAfterEvents(), false,
			review.Outcome{Status: review.StatusCanceled, Result: "Looking at the diff.", Tokens: 160}},
		{"timeout", timeout, 1000, never, false,
			review.Outcome{Status: review.StatusFailed, Error: "stopped after 1s"}},
		{"worker shut down", fakeClaude(t, true, initOK), 1000, never, true,
			review.Outcome{Status: review.StatusFailed, Error: "stopped: the worker was shut down"}},
		// claude then blocks writing to the pipe nobody reads.
		{"output line too long", fakeClaude(t, true, initOK, m1a, long, m2), 1000, never, false,
			review.Outcome{Status: review.StatusFailed, Result: "Looking at the diff.", Tokens: 160,
				Error: "stopped: could not read claude's output: bufio.Scanner: token too long"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := job
			j.TokenCap, j.Dir = tt.cap, t.TempDir()
			ctx := t.Context()
			if tt.shutdown {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx) // as on SIGTERM
				defer time.AfterFunc(time.Second, cancel).Stop()
			}
			start := time.Now()
			got := tt.claude.Run(ctx, j, tt.report)
			if got != tt.want {
				t.Errorf("Run() = %+v\nwant %+v", got, tt.want)
			}
			// The repo's own settings, hooks and CLAUDE.md must not load: the PR author controls them.
			args, _ := os.ReadFile(filepath.Join(filepath.Dir(tt.claude.Bin), "args"))
			if !strings.Contains(string(args), "--restricted --safe-mode") {
				t.Errorf("claude args = %s, want --restricted --safe-mode", args)
			}
			if time.Since(start) > 5*time.Second {
				t.Error("the run was not stopped: it waited for the fake claude to exit")
			}
		})
	}
}

// cancelAfterEvents returns a Report that asks to stop on its second call after claude's events
// were read, so only a report made while claude prints nothing can stop the run.
func cancelAfterEvents() Report {
	calls := 0
	return func(_ context.Context, tokens int) (bool, error) {
		if tokens > 0 {
			calls++
		}
		return calls >= 2, nil
	}
}

// No API key variable reaches claude, so a review can't be billed to the API.
func TestEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "subscription-token")
	env := Env()
	if !slices.Contains(env, "CLAUDE_CODE_OAUTH_TOKEN=subscription-token") {
		t.Errorf("env = %v, want the subscription token", env)
	}
	for _, v := range env {
		if strings.HasPrefix(v, "ANTHROPIC_") {
			t.Errorf("env has %s", v)
		}
	}
}

// When claude exits without a result, the error ends with the last of what it wrote to stderr.
func TestRunWithoutResult(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nhead -c 10000 /dev/zero | tr '\\0' x >&2\necho ' rate limited' >&2\nexit 3\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	c := Claude{Bin: bin, Env: []string{"PATH=" + os.Getenv("PATH")}, Timeout: 10 * time.Second, ProgressEvery: time.Second}
	job := review.Job{ID: 1, TokenCap: 1000, Dir: t.TempDir(), Prompt: "review"}
	got := c.Run(t.Context(), job, func(context.Context, int) (bool, error) { return false, nil })
	prefix := "claude stopped without a result (exit status 3): "
	if got.Status != review.StatusFailed || !strings.HasPrefix(got.Error, prefix) || !strings.HasSuffix(got.Error, "x rate limited") ||
		len(got.Error) > len(prefix)+4096 {
		t.Errorf("Run() = %s %.80q... (%d bytes), want failed with the end of stderr, at most 4096 bytes of it", got.Status, got.Error, len(got.Error))
	}
}
