// Package worker runs reviews with the claude CLI, on jobs it gets from the api.
package worker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// Env returns the only environment claude gets. Building it from scratch keeps any API key
// variable (ANTHROPIC_API_KEY, ...) away from claude, so a review can't be billed to the API.
func Env() []string {
	env := []string{"DISABLE_AUTOUPDATER=1"}
	for _, name := range []string{"PATH", "HOME", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}

// Claude runs reviews with the claude CLI.
type Claude struct {
	Bin           string        // the claude executable
	Env           []string      // claude's whole environment, see Env
	Timeout       time.Duration // the longest a review may run
	ProgressEvery time.Duration // how often the token count is reported
}

// Report sends the token count to the api and returns whether to stop.
type Report func(ctx context.Context, tokens int) (stop bool, err error)

// event holds the fields used from claude's stream-json output.
type event struct {
	Type         string `json:"type"`
	Subtype      string `json:"subtype"`
	APIKeySource string `json:"apiKeySource"` // in the init event: "none" means the subscription token
	Message      struct {
		ID      string `json:"id"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			Input         int `json:"input_tokens"`
			CacheCreation int `json:"cache_creation_input_tokens"`
			Output        int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Result           string          `json:"result"`
	IsError          bool            `json:"is_error"`
	StructuredOutput json.RawMessage `json:"structured_output"` // the answer, checked against review.OutputSchema
}

var errTimeout = errors.New("the review took too long")

// maxLine is the longest line of claude's output the worker reads.
var maxLine = 64 << 20

// Flags are the claude flags of every run in a PR's checkout, which the PR author controls: the
// review, and a session continued from it. Read, Grep and Glob are the only tools. --restricted
// ignores the settings files, so the repo's .claude/settings.json can't add hooks (which would run
// commands next to the token) or env, and it keeps the file tools inside the checkout. --safe-mode
// also ignores the repo's CLAUDE.md, skills, agents and commands.
func Flags(model, effort string) []string {
	return []string{"--model", model, "--effort", effort, "--tools", "Read,Grep,Glob",
		"--permission-mode", "dontAsk", "--restricted", "--safe-mode", "--strict-mcp-config"}
}

// Run runs one review in the PR's checkout, with Flags.
func (c Claude) Run(ctx context.Context, job review.Job, report Report) review.Outcome {
	ctx, cancel := context.WithTimeoutCause(ctx, c.Timeout, errTimeout)
	defer cancel()
	args := append([]string{"-p"}, Flags(job.Model, job.Effort)...)
	args = append(args, "--output-format", "stream-json", "--verbose", "--session-id", job.SessionID,
		"--json-schema", review.OutputSchema)
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = job.Dir, c.Env, strings.NewReader(job.Prompt)
	stderr := &tail{max: 4096}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return review.Outcome{Status: review.StatusFailed, Error: "start claude: " + err.Error()}
	}

	var (
		mu                   sync.Mutex         // the progress goroutine reads usage and may stop the run
		usage                = map[string]int{} // tokens per message: claude repeats a message's usage for each content block
		text                 string             // the latest text, kept when a run is stopped
		result               *event
		stopStatus, stopNote string
	)
	stop := func(status, note string) {
		mu.Lock()
		defer mu.Unlock()
		if stopStatus == "" {
			stopStatus, stopNote = status, note
			cancel()
		}
	}

	// Progress goes out on a timer, not on claude's output: claude prints nothing while it writes
	// a long block, such as a long thinking turn, and Cancel must still stop it.
	tick, stopTicking := context.WithCancel(ctx)
	ticking := make(chan struct{})
	go func() {
		defer close(ticking)
		t := time.NewTicker(c.ProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-tick.Done():
				return
			case <-t.C:
			}
			mu.Lock()
			tokens := sum(usage)
			mu.Unlock()
			if halt, err := report(tick, tokens); err == nil && halt {
				stop(review.StatusCanceled, "")
			}
		}
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, min(64<<10, maxLine)), maxLine) // the limit is the larger of the two
	for scanner.Scan() {
		var e event
		if json.Unmarshal(scanner.Bytes(), &e) != nil {
			continue
		}
		if e.Type == "system" && e.Subtype == "init" && e.APIKeySource != "none" {
			stop(review.StatusFailed, fmt.Sprintf("stopped: claude was not using the subscription token (apiKeySource %q)", e.APIKeySource))
		}
		mu.Lock()
		switch e.Type {
		case "assistant":
			u := e.Message.Usage
			usage[e.Message.ID] = u.Input + u.CacheCreation + u.Output
			for _, block := range e.Message.Content {
				if block.Type == "text" && block.Text != "" {
					text = block.Text
				}
			}
		case "result":
			result = &e
		}
		tokens := sum(usage)
		mu.Unlock()
		if tokens > job.TokenCap {
			stop(review.StatusCapped, fmt.Sprintf("stopped at the token cap of %d", job.TokenCap))
		}
	}
	if err := scanner.Err(); err != nil {
		// claude would block writing the rest of its output, and Wait would wait for it.
		stop(review.StatusFailed, "stopped: could not read claude's output: "+err.Error())
	}
	waitErr := cmd.Wait()
	stopTicking()
	<-ticking

	tokens := sum(usage)
	switch {
	case stopStatus != "":
		return review.Outcome{Status: stopStatus, Result: text, Error: stopNote, Tokens: tokens}
	case result != nil && !result.IsError:
		var out review.Output
		if err := json.Unmarshal(result.StructuredOutput, &out); err != nil || out.Review == "" {
			// claude may give up on the schema: its last text is all there is, without drafts.
			if strings.TrimSpace(result.Result) == "" {
				return review.Outcome{Status: review.StatusFailed, Error: "claude returned no answer", Tokens: tokens}
			}
			return review.Outcome{Status: review.StatusDone, Result: result.Result, Tokens: tokens,
				Error: "claude didn't return its answer in the expected format: no comments were drafted for GitHub"}
		}
		drafts := out.Drafts()
		return review.Outcome{Status: review.StatusDone, Result: out.Review, Tokens: tokens, Drafts: &drafts}
	case result != nil:
		return review.Outcome{Status: review.StatusFailed, Result: text, Error: result.Result, Tokens: tokens}
	case errors.Is(context.Cause(ctx), errTimeout):
		return review.Outcome{Status: review.StatusFailed, Result: text, Error: "stopped after " + c.Timeout.String(), Tokens: tokens}
	case ctx.Err() != nil:
		return review.Outcome{Status: review.StatusFailed, Result: text, Error: "stopped: the worker was shut down", Tokens: tokens}
	default:
		return review.Outcome{Status: review.StatusFailed, Result: text, Tokens: tokens,
			Error: fmt.Sprintf("claude stopped without a result (%v): %s", waitErr, stderr.String())}
	}
}

func sum(m map[string]int) int {
	total := 0
	for _, n := range m {
		total += n
	}
	return total
}

// tail keeps the last max bytes written to it.
type tail struct {
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string { return strings.TrimSpace(string(t.buf)) }
