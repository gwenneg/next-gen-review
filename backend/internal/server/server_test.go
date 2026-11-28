package server

import (
	"context"
	"encoding/json"
	"errors"
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

type fakeStore struct{ repos []string }

func (f *fakeStore) Queue(context.Context) ([]github.PullRequest, error) { return nil, nil }
func (f *fakeStore) WatchedRepos(context.Context) ([]string, error)      { return f.repos, nil }
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
	}
	return review.Review{}, store.ErrNotInQueue
}
func (f *fakeReviews) Reviews(_ context.Context, activeOnly bool) ([]review.Review, error) {
	f.activeOnly = activeOnly
	return nil, nil
}
func (f *fakeReviews) CancelReview(_ context.Context, id int64) (bool, error) {
	return id == 9, nil
}

type fakeBroker struct {
	wakes     int
	connected bool
}

func (f *fakeBroker) Wake() { f.wakes++ }
func (f *fakeBroker) Next(context.Context, time.Duration) (review.Job, bool, error) {
	return review.Job{}, false, nil
}
func (f *fakeBroker) Progress(context.Context, int64, int) (bool, error) { return false, nil }
func (f *fakeBroker) Finish(context.Context, int64, review.Outcome)      {}
func (f *fakeBroker) WorkerConnected() bool                              { return f.connected }

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
		{"cancel", "/api/reviews/9/cancel", "", false, http.StatusNoContent, false, true},
		{"cancel an inactive review", "/api/reviews/3/cancel", "", false, http.StatusNotFound, false, false},
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
