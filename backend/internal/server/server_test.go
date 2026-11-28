package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/gwenneg/next-gen-review/backend/internal/events"
	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/queue"
	"github.com/gwenneg/next-gen-review/backend/internal/review"
	"github.com/gwenneg/next-gen-review/backend/internal/store"
)

type fakeDB struct{ err error }

func (f fakeDB) Ping(context.Context) error { return f.err }

type fakeStore struct {
	repos []string
	err   error // returned by the reads
}

func (f *fakeStore) Queue(context.Context) ([]github.PullRequest, error) { return nil, f.err }
func (f *fakeStore) WatchedRepos(context.Context) ([]string, error)      { return f.repos, f.err }
func (f *fakeStore) AddWatchedRepo(_ context.Context, name string) error {
	f.repos = append(f.repos, name)
	return nil
}
func (f *fakeStore) RemoveWatchedRepo(_ context.Context, name string) (bool, error) {
	i := slices.Index(f.repos, name)
	if i < 0 {
		return false, nil
	}
	f.repos = slices.Delete(f.repos, i, i+1)
	return true, nil
}

type fakePoller struct {
	wakes   int
	pollErr error // the poll context's error when Poll ran
}

func (f *fakePoller) Status() queue.Status { return queue.Status{} }
func (f *fakePoller) Poll(ctx context.Context) error {
	f.pollErr = ctx.Err()
	return nil
}
func (f *fakePoller) Wake() { f.wakes++ }

// fakeRepos refuses "o/private", fails on "o/github-down" as if GitHub were down, and returns other
// names in upper case like GitHub's spelling.
type fakeRepos struct{}

func (fakeRepos) PublicRepo(_ context.Context, name string) (string, error) {
	switch name {
	case "o/private":
		return "", github.InvalidRepoError{Msg: "o/private is private"}
	case "o/github-down":
		return "", errors.New("HTTP 502: Bad Gateway")
	}
	return strings.ToUpper(name), nil
}

// fakeReviews knows one queued PR, "PR_1", which has no active review yet, and one active review, 9.
type fakeReviews struct {
	err        error // returned by Reviews
	created    int
	activeOnly bool // the last list asked for active reviews only
}

func (f *fakeReviews) CreateReview(_ context.Context, req review.Request) (review.Review, error) {
	switch req.PRID {
	case "PR_1":
		f.created++
		return review.Review{ID: 1, PRID: req.PRID, Status: review.StatusQueued}, nil
	case "PR_active":
		return review.Review{}, store.ErrActiveReview
	case "PR_db_down":
		return review.Review{}, errors.New("conn closed")
	}
	return review.Review{}, store.ErrNotInQueue
}
func (f *fakeReviews) Reviews(_ context.Context, activeOnly bool) ([]review.Review, error) {
	f.activeOnly = activeOnly
	return nil, f.err
}
func (f *fakeReviews) CancelReview(_ context.Context, id int64) (bool, error) {
	if id == 5 {
		return false, errors.New("conn closed")
	}
	return id == 9, nil
}

type fakeBroker struct {
	wakes     int
	connected bool
	job       bool  // Next returns a job
	err       error // returned by Next and Progress
	finishErr error // the outcome isn't stored
	finished  []review.Outcome
}

func (f *fakeBroker) Wake() { f.wakes++ }
func (f *fakeBroker) Next(context.Context, time.Duration) (review.Job, bool, error) {
	return review.Job{ID: 3}, f.job, f.err
}
func (f *fakeBroker) Progress(context.Context, int64, int) (bool, error) { return true, f.err }
func (f *fakeBroker) Finish(_ context.Context, _ int64, o review.Outcome) error {
	f.finished = append(f.finished, o)
	return f.finishErr
}
func (f *fakeBroker) WorkerConnected() bool { return f.connected }
func (f *fakeBroker) Session(_ context.Context, id int64) (review.Resume, error) {
	return review.Resume{SessionID: "s"}, continueErr(id)
}
func (f *fakeBroker) PrepareContinue(_ context.Context, id int64) (review.Resume, error) {
	return review.Resume{SessionID: "s"}, continueErr(id)
}
func (f *fakeBroker) RemoveCheckout(_ context.Context, id int64, _ string) error {
	return continueErr(id)
}

// continueErr is the broker's answer about review id: 4 ended, 5 is running, 6 doesn't exist,
// GitHub fails for 7, 8 is being downloaded for another continue, and the database is down for 9.
func continueErr(id int64) error {
	return map[int64]error{5: review.ErrActive, 6: review.ErrUnknownReview, 7: fmt.Errorf("%w: HTTP 502", review.ErrDownload),
		8: review.ErrDownloading, 9: errors.New("conn closed")}[id]
}

type fakeEvents struct{ ch chan events.Event }

func (f fakeEvents) Subscribe() (<-chan events.Event, func()) { return f.ch, func() {} }

func newDeps() Deps {
	return Deps{DB: fakeDB{}, Store: &fakeStore{}, Poller: &fakePoller{}, Repos: fakeRepos{}, Events: fakeEvents{make(chan events.Event, 1)},
		Reviews: &fakeReviews{}, Broker: &fakeBroker{}}
}

func TestHealth(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		dbErr  error
		status int
		body   string
	}{
		{"database up", "127.0.0.1:8080", nil, http.StatusOK, `"database":"ok"`},
		{"database up, localhost", "localhost:8080", nil, http.StatusOK, `"database":"ok"`},
		{"database down", "127.0.0.1:8080", errors.New("connection refused"), http.StatusServiceUnavailable, `"database":"unreachable"`},
		{"dns rebinding", "attacker.example:8080", nil, http.StatusForbidden, "host not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDeps()
			d.DB = fakeDB{tt.dbErr}
			req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
			req.Host = tt.host
			rec := httptest.NewRecorder()
			New(d).ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			if !strings.Contains(rec.Body.String(), tt.body) {
				t.Errorf("body = %s, want it to contain %s", rec.Body.String(), tt.body)
			}
		})
	}
}

func TestWatchedRepos(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		path      string
		body      string
		crossSite bool
		status    int
		repos     []string // stored repos after the request
	}{
		{"add", http.MethodPost, "/api/watched-repos", `{"repo":"o/r"}`, false, http.StatusCreated, []string{"o/existing", "O/R"}},
		{"add private", http.MethodPost, "/api/watched-repos", `{"repo":"o/private"}`, false, http.StatusBadRequest, []string{"o/existing"}},
		{"add while GitHub is down", http.MethodPost, "/api/watched-repos", `{"repo":"o/github-down"}`, false, http.StatusBadGateway, []string{"o/existing"}},
		{"add bad json", http.MethodPost, "/api/watched-repos", `o/r`, false, http.StatusBadRequest, []string{"o/existing"}},
		{"add from another site (CSRF)", http.MethodPost, "/api/watched-repos", `{"repo":"o/r"}`, true, http.StatusForbidden, []string{"o/existing"}},
		{"remove", http.MethodDelete, "/api/watched-repos/o/existing", "", false, http.StatusNoContent, []string{}},
		{"remove unknown", http.MethodDelete, "/api/watched-repos/o/unknown", "", false, http.StatusNotFound, []string{"o/existing"}},
		{"remove from another site (CSRF)", http.MethodDelete, "/api/watched-repos/o/existing", "", true, http.StatusForbidden, []string{"o/existing"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDeps()
			st := &fakeStore{repos: []string{"o/existing"}}
			poller := &fakePoller{}
			d.Store, d.Poller = st, poller
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Host = "127.0.0.1:8080"
			if tt.crossSite {
				req.Header.Set("Sec-Fetch-Site", "cross-site")
				req.Header.Set("Origin", "https://evil.example")
			}
			rec := httptest.NewRecorder()
			New(d).ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.status, rec.Body.String())
			}
			if !slices.Equal(st.repos, tt.repos) {
				t.Errorf("repos = %v, want %v", st.repos, tt.repos)
			}
			if changed := tt.status < 300; changed != (poller.wakes == 1) {
				t.Errorf("poller woken %d times, want a wake only when the repos change", poller.wakes)
			}
		})
	}
}

func TestReviews(t *testing.T) {
	valid := `{"prId":"PR_1","model":"sonnet","effort":"medium","tokenCap":500000,"context":"focus on retries"}`
	tests := []struct {
		name      string
		path      string
		body      string
		crossSite bool
		status    int
		created   bool // a review was created
		woke      bool // the broker was told to look at the reviews again
	}{
		{"start", "/api/reviews", valid, false, http.StatusCreated, true, true},
		{"start with a bad option", "/api/reviews", strings.Replace(valid, "sonnet", "gpt", 1), false, http.StatusBadRequest, false, false},
		{"start a PR not in the queue", "/api/reviews", strings.Replace(valid, "PR_1", "PR_unknown", 1), false, http.StatusNotFound, false, false},
		{"start while one is active", "/api/reviews", strings.Replace(valid, "PR_1", "PR_active", 1), false, http.StatusConflict, false, false},
		{"start from another site (CSRF)", "/api/reviews", valid, true, http.StatusForbidden, false, false},
		{"start while the database is down", "/api/reviews", strings.Replace(valid, "PR_1", "PR_db_down", 1), false, http.StatusInternalServerError, false, false},
		{"start with bad json", "/api/reviews", "{", false, http.StatusBadRequest, false, false},
		{"cancel", "/api/reviews/9/cancel", "", false, http.StatusNoContent, false, true},
		{"cancel an inactive review", "/api/reviews/3/cancel", "", false, http.StatusNotFound, false, false},
		{"cancel a bad id", "/api/reviews/x/cancel", "", false, http.StatusBadRequest, false, false},
		{"cancel while the database is down", "/api/reviews/5/cancel", "", false, http.StatusInternalServerError, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDeps()
			reviews, broker := d.Reviews.(*fakeReviews), d.Broker.(*fakeBroker)
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Host = "127.0.0.1:8080"
			if tt.crossSite {
				req.Header.Set("Sec-Fetch-Site", "cross-site")
				req.Header.Set("Origin", "https://evil.example")
			}
			rec := httptest.NewRecorder()
			New(d).ServeHTTP(rec, req)
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.status, rec.Body.String())
			}
			if created, woke := reviews.created > 0, broker.wakes > 0; created != tt.created || woke != tt.woke {
				t.Errorf("created a review: %v, woke the broker: %v; want %v, %v", created, woke, tt.created, tt.woke)
			}
		})
	}
}

// The page warns about queued reviews when the worker is missing, and reloads only the active
// reviews on progress reports.
func TestListReviews(t *testing.T) {
	for _, connected := range []bool{true, false} {
		d := newDeps()
		d.Broker.(*fakeBroker).connected = connected
		path := "/api/reviews"
		if connected {
			path += "?active=true"
		}
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		New(d).ServeHTTP(rec, req)
		var body struct{ WorkerConnected bool }
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.WorkerConnected != connected {
			t.Errorf("body = %s (%v), want workerConnected %v", rec.Body.String(), err, connected)
		}
		if got := d.Reviews.(*fakeReviews).activeOnly; got != connected {
			t.Errorf("GET %s listed active reviews only: %v", path, got)
		}
	}
}

// The read-only endpoints the page loads, and their answer while the database is down.
func TestRead(t *testing.T) {
	for path, want := range map[string]string{
		"/api/watched-repos":  `{"repos":["o/existing"]}`,
		"/api/review-options": `"sonnet"`,
		"/api/queue":          `"items":`,
		"/api/reviews":        `"reviews":`,
	} {
		for _, err := range []error{nil, errors.New("conn closed")} {
			d := newDeps()
			d.Store, d.Reviews = &fakeStore{repos: []string{"o/existing"}, err: err}, &fakeReviews{err: err}
			req := httptest.NewRequest(http.MethodGet, path, nil)
			req.Host = "127.0.0.1:8080"
			rec := httptest.NewRecorder()
			New(d).ServeHTTP(rec, req)
			status := http.StatusOK
			if err != nil && path != "/api/review-options" {
				status, want = http.StatusInternalServerError, `"error":"could not read`
			}
			if rec.Code != status || !strings.Contains(rec.Body.String(), want) {
				t.Errorf("GET %s with store error %v = %d %s, want %d with %s", path, err, rec.Code, rec.Body.String(), status, want)
			}
		}
	}
}

// The worker's API refuses bad requests and reports the broker's errors. The JSON of the happy
// paths is tested with the worker's own client, in package worker.
func TestInternal(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name     string
		method   string
		path     string
		body     string
		broker   fakeBroker
		ctx      context.Context
		status   int
		finished int // outcomes passed to the broker
	}{
		{"no job", http.MethodGet, "/internal/jobs/next", "", fakeBroker{}, nil, http.StatusNoContent, 0},
		{"next fails", http.MethodGet, "/internal/jobs/next", "", fakeBroker{err: errors.New("db down")}, nil, http.StatusInternalServerError, 0},
		{"worker left", http.MethodGet, "/internal/jobs/next", "", fakeBroker{err: context.Canceled}, canceled, http.StatusNoContent, 0},
		{"progress", http.MethodPost, "/internal/jobs/3/progress", `{"tokens":5}`, fakeBroker{}, nil, http.StatusOK, 0},
		{"progress, bad id", http.MethodPost, "/internal/jobs/x/progress", `{"tokens":5}`, fakeBroker{}, nil, http.StatusBadRequest, 0},
		{"progress, bad body", http.MethodPost, "/internal/jobs/3/progress", `5`, fakeBroker{}, nil, http.StatusBadRequest, 0},
		{"progress fails", http.MethodPost, "/internal/jobs/3/progress", `{"tokens":5}`, fakeBroker{err: errors.New("db down")}, nil, http.StatusInternalServerError, 0},
		{"finish", http.MethodPost, "/internal/jobs/3/finish", `{"status":"done"}`, fakeBroker{}, nil, http.StatusNoContent, 1},
		{"finish not stored yet", http.MethodPost, "/internal/jobs/3/finish", `{"status":"done"}`, fakeBroker{finishErr: errors.New("db down")}, nil, http.StatusServiceUnavailable, 1},
		{"finish, bad id", http.MethodPost, "/internal/jobs/x/finish", `{"status":"done"}`, fakeBroker{}, nil, http.StatusBadRequest, 0},
		{"finish, bad body", http.MethodPost, "/internal/jobs/3/finish", `done`, fakeBroker{}, nil, http.StatusBadRequest, 0},
		{"continue", http.MethodPost, "/internal/reviews/4/continue", "", fakeBroker{}, nil, http.StatusOK, 0},
		{"continue a running review", http.MethodPost, "/internal/reviews/5/continue", "", fakeBroker{}, nil, http.StatusConflict, 0},
		{"continue an unknown review", http.MethodPost, "/internal/reviews/6/continue", "", fakeBroker{}, nil, http.StatusNotFound, 0},
		{"continue while GitHub fails", http.MethodPost, "/internal/reviews/7/continue", "", fakeBroker{}, nil, http.StatusBadGateway, 0},
		{"continue during another one's download", http.MethodPost, "/internal/reviews/8/continue", "", fakeBroker{}, nil, http.StatusConflict, 0},
		{"continue while the database is down", http.MethodPost, "/internal/reviews/9/continue", "", fakeBroker{}, nil, http.StatusInternalServerError, 0},
		{"session", http.MethodGet, "/internal/reviews/4/session", "", fakeBroker{}, nil, http.StatusOK, 0},
		{"session of a running review", http.MethodGet, "/internal/reviews/5/session", "", fakeBroker{}, nil, http.StatusConflict, 0},
		{"delete the checkout while the database is down", http.MethodDelete, "/internal/reviews/9/checkout", "", fakeBroker{}, nil, http.StatusInternalServerError, 0},
		{"continue, bad id", http.MethodPost, "/internal/reviews/x/continue", "", fakeBroker{}, nil, http.StatusBadRequest, 0},
		{"delete the checkout", http.MethodDelete, "/internal/reviews/4/checkout", "", fakeBroker{}, nil, http.StatusNoContent, 0},
		{"delete a running review's checkout", http.MethodDelete, "/internal/reviews/5/checkout", "", fakeBroker{}, nil, http.StatusConflict, 0},
		{"delete the checkout, bad id", http.MethodDelete, "/internal/reviews/x/checkout", "", fakeBroker{}, nil, http.StatusBadRequest, 0},
		{"finish, too large", http.MethodPost, "/internal/jobs/3/finish", `{"result":"` + strings.Repeat("a", 4<<20) + `"}`, fakeBroker{}, nil, http.StatusBadRequest, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := tt.broker
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.ctx != nil {
				req = req.WithContext(tt.ctx)
			}
			rec := httptest.NewRecorder()
			NewInternal(&b).ServeHTTP(rec, req)
			if rec.Code != tt.status || len(b.finished) != tt.finished {
				t.Errorf("status = %d (body %s), outcomes passed on: %d; want %d, %d", rec.Code, rec.Body.String(), len(b.finished), tt.status, tt.finished)
			}
		})
	}
}

// A client leaving mid-Refresh must not cancel the poll, or "context canceled" would be stored
// as GitHub's error.
func TestRefreshSurvivesClientLeaving(t *testing.T) {
	d := newDeps()
	poller := d.Poller.(*fakePoller)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/api/queue/refresh", nil)
	req.Host = "127.0.0.1:8080"
	New(d).ServeHTTP(httptest.NewRecorder(), req)
	if poller.pollErr != nil {
		t.Errorf("poll context error = %v, want none", poller.pollErr)
	}
}

// Guards against a page on another site using the API through the user's browser.
func TestWebSocket(t *testing.T) {
	tests := []struct {
		name   string
		host   string
		origin string
		ok     bool
	}{
		{"same origin", "", "", true},
		{"other website", "", "https://evil.example", false},
		{"dns rebinding", "attacker.example:8080", "http://attacker.example:8080", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newDeps()
			ev := d.Events.(fakeEvents)
			srv := httptest.NewServer(New(d))
			defer srv.Close()
			origin := tt.origin
			if origin == "" {
				origin = srv.URL
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/ws"
			conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{Host: tt.host, HTTPHeader: http.Header{"Origin": {origin}}})
			if !tt.ok {
				if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
					t.Fatalf("want 403, got err=%v resp=%v", err, resp)
				}
				return
			}
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer conn.CloseNow()
			var hello map[string]any
			if err := wsjson.Read(ctx, conn, &hello); err != nil || hello["type"] != "hello" {
				t.Fatalf("first message = %v, err = %v", hello, err)
			}
			ev.ch <- events.Event{Seq: 7, Type: "queue.changed"}
			var got events.Event
			if err := wsjson.Read(ctx, conn, &got); err != nil || got != (events.Event{Seq: 7, Type: "queue.changed"}) {
				t.Fatalf("forwarded event = %+v, err = %v", got, err)
			}
		})
	}
}
