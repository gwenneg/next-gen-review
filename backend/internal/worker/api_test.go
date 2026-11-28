package worker

import (
	"context"
	"errors"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
	"github.com/gwenneg/next-gen-review/backend/internal/server"
)

// fakeBroker stands in for the api's broker behind its real internal handlers.
type fakeBroker struct {
	job       *review.Job // returned by Next, if any
	err       error       // returned by Next and Progress
	stop      bool
	progress  [2]int64 // the last report: id, tokens
	finished  map[int64]review.Outcome
	finishErr error    // the outcome isn't stored
	removed   []string // the downloads deleted
}

func (f *fakeBroker) Wake() {}
func (f *fakeBroker) Next(context.Context, time.Duration) (review.Job, bool, error) {
	if f.job == nil {
		return review.Job{}, false, f.err
	}
	return *f.job, true, f.err
}
func (f *fakeBroker) Progress(_ context.Context, id int64, tokens int) (bool, error) {
	f.progress = [2]int64{id, int64(tokens)}
	return f.stop, f.err
}
func (f *fakeBroker) Finish(_ context.Context, id int64, o review.Outcome) error {
	f.finished[id] = o
	return f.finishErr
}
func (f *fakeBroker) WorkerConnected() bool { return true }
func (f *fakeBroker) Session(_ context.Context, id int64) (review.Resume, error) {
	return review.Resume{SessionID: "s", Model: "haiku", Effort: "low"}, nil
}
func (f *fakeBroker) PrepareContinue(_ context.Context, id int64) (review.Resume, error) {
	if id != 3 {
		return review.Resume{}, review.ErrActive
	}
	return review.Resume{SessionID: "s", Model: "haiku", Effort: "low", Dir: "/checkouts/3", Download: "d&1"}, nil
}
func (f *fakeBroker) RemoveCheckout(_ context.Context, id int64, download string) error {
	f.removed = append(f.removed, download)
	return nil
}

// The worker's client and the api's internal handlers agree on paths, statuses and JSON.
func TestAPI(t *testing.T) {
	b := &fakeBroker{finished: map[int64]review.Outcome{}}
	srv := httptest.NewServer(server.NewInternal(b))
	defer srv.Close()
	api := NewAPI(srv.URL)
	ctx := t.Context()

	if _, ok, err := api.Next(ctx); ok || err != nil {
		t.Errorf("Next() with no job = %v, %v; want false", ok, err)
	}
	job := review.Job{ID: 3, Model: "sonnet", Effort: "high", TokenCap: 1000, SessionID: "s", Dir: "/checkouts/3", Prompt: "review"}
	b.job = &job
	if got, ok, err := api.Next(ctx); !ok || err != nil || !reflect.DeepEqual(got, job) {
		t.Errorf("Next() = %+v, %v, %v; want %+v", got, ok, err, job)
	}

	b.stop = true
	if stop, err := api.Progress(ctx, 3, 1500); !stop || err != nil || b.progress != [2]int64{3, 1500} {
		t.Errorf("Progress() = %v, %v, broker got %v; want stop and the report", stop, err, b.progress)
	}
	o := review.Outcome{Status: review.StatusDone, Result: "## Findings", Error: "", Tokens: 1500}
	if err := api.Finish(ctx, 3, o); err != nil || b.finished[3] != o {
		t.Errorf("Finish() = %v, broker got %+v; want %+v", err, b.finished[3], o)
	}

	if got, err := api.Session(ctx, 3); err != nil || got != (review.Resume{SessionID: "s", Model: "haiku", Effort: "low"}) {
		t.Errorf("Session() = %+v, %v", got, err)
	}
	want := review.Resume{SessionID: "s", Model: "haiku", Effort: "low", Dir: "/checkouts/3", Download: "d&1"}
	if got, err := api.Continue(ctx, 3); err != nil || got != want {
		t.Errorf("Continue() = %+v, %v; want %+v", got, err, want)
	}
	if _, err := api.Continue(ctx, 5); err == nil || !strings.Contains(err.Error(), "409 Conflict: the review is still active") {
		t.Errorf("Continue() of a running review = %v, want the api's message", err)
	}
	if err := api.RemoveCheckout(ctx, 3, "d&1"); err != nil || !slices.Equal(b.removed, []string{"d&1"}) {
		t.Errorf("RemoveCheckout() = %v, broker removed %v", err, b.removed)
	}

	b.finishErr = errors.New("database down")
	var status *StatusError
	if err := api.Finish(ctx, 3, o); !errors.As(err, &status) || status.Code != 503 {
		t.Errorf("Finish() of an outcome the api couldn't store = %v, want a 503 to retry on", err)
	}

	b.err = errors.New("database down")
	if _, _, err := api.Next(ctx); err == nil {
		t.Error("Next() hid the api's error")
	}
	if _, err := api.Progress(ctx, 3, 1); err == nil {
		t.Error("Progress() hid the api's error")
	}
}
