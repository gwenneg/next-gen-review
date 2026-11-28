package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// API calls the api's internal endpoints.
type API struct {
	Base string // for example http://api:8081
	HTTP *http.Client
}

// NewAPI returns a client for the api at base.
func NewAPI(base string) *API {
	return &API{Base: base, HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// StatusError is an answer from the api other than a success.
type StatusError struct {
	Method, Path, Status string
	Code                 int
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s %s: %s", e.Method, e.Path, e.Status) }

func (a *API) call(ctx context.Context, method, path string, in, out any) (int, error) {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return 0, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, a.Base+path, &body)
	if err != nil {
		return 0, err
	}
	resp, err := a.HTTP.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return resp.StatusCode, &StatusError{Method: method, Path: path, Status: resp.Status, Code: resp.StatusCode}
	}
	if out != nil && resp.StatusCode != http.StatusNoContent {
		return resp.StatusCode, json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode, nil
}

// Next waits for the next job. It returns false when the api had none to give.
func (a *API) Next(ctx context.Context) (review.Job, bool, error) {
	var job review.Job
	status, err := a.call(ctx, http.MethodGet, "/internal/jobs/next", nil, &job)
	return job, err == nil && status == http.StatusOK, err
}

// Progress reports the token count and returns whether to stop.
func (a *API) Progress(ctx context.Context, id int64, tokens int) (bool, error) {
	var out struct {
		Stop bool `json:"stop"`
	}
	_, err := a.call(ctx, http.MethodPost, fmt.Sprintf("/internal/jobs/%d/progress", id), map[string]int{"tokens": tokens}, &out)
	return out.Stop, err
}

// Finish reports how the job ended. An error means the api didn't store it.
func (a *API) Finish(ctx context.Context, id int64, o review.Outcome) error {
	_, err := a.call(ctx, http.MethodPost, fmt.Sprintf("/internal/jobs/%d/finish", id), o, nil)
	return err
}
