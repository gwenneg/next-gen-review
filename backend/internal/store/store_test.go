package store

import (
	"context"
	"errors"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gwenneg/next-gen-review/backend/internal/dbtest"
	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) { os.Exit(dbtest.Main(m, &pool)) }

func newStore(t *testing.T) *Store {
	dbtest.Reset(t, pool)
	return New(pool)
}

// events returns the types of the stored events, oldest first.
func events(t *testing.T) []string {
	rows, err := pool.Query(t.Context(), `SELECT type FROM events ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	types, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	return types
}

// failEvents makes every event insert fail until the test ends, as a database error would.
func failEvents(t *testing.T) {
	_, err := pool.Exec(t.Context(), `CREATE FUNCTION fail_event() RETURNS trigger LANGUAGE plpgsql AS
		$$BEGIN RAISE EXCEPTION 'events are failing'; END$$;
		CREATE TRIGGER fail_event BEFORE INSERT ON events FOR EACH ROW EXECUTE FUNCTION fail_event()`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // t.Context() is already canceled here
		if _, err := pool.Exec(context.Background(), `DROP TRIGGER fail_event ON events; DROP FUNCTION fail_event()`); err != nil {
			t.Error(err)
		}
	})
}

var created = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func pr(id string, age time.Duration) github.PullRequest {
	return github.PullRequest{ID: id, Repo: "o/r", Number: 7, Title: "Add retries", URL: "https://github.com/o/r/pull/7",
		Author: "a", CreatedAt: created.Add(-age), UpdatedAt: created, Additions: 3, Deletions: 1, ChangedFiles: 2,
		HeadSHA: "abc", CIState: "SUCCESS", Reasons: []github.Reason{{Kind: "requested", ViaTeam: true, Teams: []string{"o/t"}}}}
}

func TestWatchedRepos(t *testing.T) {
	s := newStore(t)
	for _, name := range []string{"o/zeta", "O/alpha", "o/zeta"} { // the duplicate is ignored
		if err := s.AddWatchedRepo(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.WatchedRepos(t.Context()); err != nil || !slices.Equal(got, []string{"O/alpha", "o/zeta"}) {
		t.Errorf("WatchedRepos() = %v, %v; want them sorted, ignoring case", got, err)
	}
	if removed, err := s.RemoveWatchedRepo(t.Context(), "o/ALPHA"); err != nil || !removed {
		t.Errorf("RemoveWatchedRepo(another case) = %v, %v; want it removed", removed, err)
	}
	if removed, err := s.RemoveWatchedRepo(t.Context(), "o/missing"); err != nil || removed {
		t.Errorf("RemoveWatchedRepo(missing) = %v, %v; want false", removed, err)
	}
}

func TestQueue(t *testing.T) {
	s := newStore(t)
	if err := s.ReplaceQueue(t.Context(), []github.PullRequest{pr("PR_old", 0)}); err != nil {
		t.Fatal(err)
	}
	queue := []github.PullRequest{pr("PR_new", time.Hour), pr("PR_newer", 2*time.Hour)}
	if err := s.ReplaceQueue(t.Context(), queue); err != nil {
		t.Fatal(err)
	}
	got, err := s.Queue(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "PR_newer" || got[1].ID != "PR_new" {
		t.Fatalf("Queue() = %+v, want only the last queue, oldest first", got)
	}
	want := queue[1]
	if g := got[0]; g.Title != want.Title || !g.CreatedAt.Equal(want.CreatedAt) || g.HeadSHA != want.HeadSHA ||
		g.CIState != want.CIState || g.ChangedFiles != want.ChangedFiles || !slices.EqualFunc(g.Reasons, want.Reasons,
		func(a, b github.Reason) bool {
			return a.Kind == b.Kind && a.ViaTeam == b.ViaTeam && slices.Equal(a.Teams, b.Teams)
		}) {
		t.Errorf("Queue()[0] = %+v, want %+v", g, want)
	}
	if err := s.AddEvent(t.Context(), EventQueueChanged); err != nil { // as the poller does for a poll error
		t.Fatal(err)
	}
	if got := events(t); !slices.Equal(got, []string{EventQueueChanged, EventQueueChanged, EventQueueChanged}) {
		t.Errorf("events = %v, want one per replace, and the added one", got)
	}
}

var request = review.Request{PRID: "PR_1", Model: "sonnet", Effort: "high", TokenCap: 1000, Context: "focus on retries"}

// A review goes from queued to done, and the PR can be reviewed again after.
func TestReviewLifecycle(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	if _, err := s.CreateReview(ctx, request); !errors.Is(err, ErrNotInQueue) {
		t.Fatalf("CreateReview() before the PR is queued = %v, want ErrNotInQueue", err)
	}
	if err := s.ReplaceQueue(ctx, []github.PullRequest{pr("PR_1", 0)}); err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateReview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != review.StatusQueued || r.HeadSHA != "abc" || r.Repo != "o/r" || r.Context != request.Context || r.SessionID == "" {
		t.Errorf("CreateReview() = %+v, want a queued review of the queue's commit", r)
	}
	if _, err := s.CreateReview(ctx, request); !errors.Is(err, ErrActiveReview) {
		t.Errorf("a second CreateReview() = %v, want ErrActiveReview", err)
	}

	claimed, ok, err := s.ClaimReview(ctx)
	if err != nil || !ok || claimed.ID != r.ID || claimed.Status != review.StatusPreparing || claimed.StartedAt == nil {
		t.Fatalf("ClaimReview() = %+v, %v, %v; want the review, preparing", claimed, ok, err)
	}
	if _, ok, err := s.ClaimReview(ctx); ok || err != nil {
		t.Errorf("ClaimReview() with nothing queued = %v, %v; want false", ok, err)
	}
	if started, err := s.StartReview(ctx, r.ID, "the prompt"); !started || err != nil {
		t.Fatalf("StartReview() = %v, %v", started, err)
	}
	if stop, err := s.ReviewProgress(ctx, r.ID, 500); stop || err != nil {
		t.Errorf("ReviewProgress() = %v, %v; want to go on", stop, err)
	}
	if err := s.FinishReview(ctx, r.ID, review.Outcome{Status: review.StatusDone, Result: "## Findings", Tokens: 600}); err != nil {
		t.Fatal(err)
	}
	// The worker's late report of an ended review is refused, and the outcome stays.
	if err := s.FinishReview(ctx, r.ID, review.Outcome{Status: review.StatusFailed}); !errors.Is(err, review.ErrNotActive) {
		t.Errorf("FinishReview() of an ended review = %v, want ErrNotActive", err)
	}
	if stop, err := s.ReviewProgress(ctx, r.ID, 700); !stop || err != nil {
		t.Errorf("ReviewProgress() of an ended review = %v, %v; want stop", stop, err)
	}
	if stop, err := s.ReviewProgress(ctx, 999, 1); !stop || err != nil {
		t.Errorf("ReviewProgress() of an unknown review = %v, %v; want stop", stop, err)
	}

	list, err := s.Reviews(ctx, false)
	if err != nil || len(list) != 1 {
		t.Fatalf("Reviews() = %+v, %v", list, err)
	}
	if got := list[0]; got.Status != review.StatusDone || got.Result != "## Findings" || got.TokensUsed != 700 || got.FinishedAt == nil {
		t.Errorf("Reviews()[0] = %+v, want done with the worker's result", got)
	}
	again, err := s.CreateReview(ctx, request)
	if err != nil {
		t.Fatalf("CreateReview() after the review ended = %v", err)
	}
	if list, err := s.Reviews(ctx, false); err != nil || len(list) != 2 || list[0].ID != again.ID {
		t.Errorf("Reviews() = %+v, %v; want both, newest first", list, err)
	}
	if list, err := s.Reviews(ctx, true); err != nil || len(list) != 1 || list[0].ID != again.ID {
		t.Errorf("Reviews(active only) = %+v, %v; want only the queued one", list, err)
	}
	want := []string{EventQueueChanged, review.EventChanged, review.EventChanged, review.EventChanged, review.EventChanged, review.EventChanged}
	if got := events(t); !slices.Equal(got, want) {
		t.Errorf("events = %v, want %v: progress reports aren't stored", got, want)
	}
}

func TestCancelReview(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	if err := s.ReplaceQueue(ctx, []github.PullRequest{pr("PR_1", 0)}); err != nil {
		t.Fatal(err)
	}
	// A queued review is canceled at once.
	r, err := s.CreateReview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CancelReview(ctx, r.ID); !ok || err != nil {
		t.Fatalf("CancelReview(queued) = %v, %v", ok, err)
	}
	if list, _ := s.Reviews(ctx, false); list[0].Status != review.StatusCanceled || list[0].FinishedAt == nil {
		t.Errorf("review = %+v, want canceled", list[0])
	}
	if ok, err := s.CancelReview(ctx, r.ID); ok || err != nil {
		t.Errorf("CancelReview(canceled) = %v, %v; want false", ok, err)
	}

	// A preparing review is only flagged: the broker skips it.
	r, _ = s.CreateReview(ctx, request)
	if _, _, err := s.ClaimReview(ctx); err != nil {
		t.Fatal(err)
	}
	if c, err := s.CancelRequested(ctx, r.ID); c || err != nil {
		t.Errorf("CancelRequested() before the cancel = %v, %v", c, err)
	}
	if ok, err := s.CancelReview(ctx, r.ID); !ok || err != nil {
		t.Fatalf("CancelReview(preparing) = %v, %v", ok, err)
	}
	if c, err := s.CancelRequested(ctx, r.ID); !c || err != nil {
		t.Errorf("CancelRequested() = %v, %v; want true", c, err)
	}
	if started, err := s.StartReview(ctx, r.ID, "prompt"); started || err != nil {
		t.Errorf("StartReview(canceled) = %v, %v; want false", started, err)
	}

	// A running review is stopped by the worker's next report.
	if err := s.FinishReview(ctx, r.ID, review.Outcome{Status: review.StatusCanceled}); err != nil {
		t.Fatal(err)
	}
	r, _ = s.CreateReview(ctx, request)
	_, _, _ = s.ClaimReview(ctx)
	_, _ = s.StartReview(ctx, r.ID, "prompt")
	if ok, err := s.CancelReview(ctx, r.ID); !ok || err != nil {
		t.Fatalf("CancelReview(running) = %v, %v", ok, err)
	}
	if stop, err := s.ReviewProgress(ctx, r.ID, 10); !stop || err != nil {
		t.Errorf("ReviewProgress() after a cancel = %v, %v; want stop", stop, err)
	}
}

// setUp queues PR_1 to PR_4 and returns a running review of PR_1, a preparing one of PR_2 and a
// queued one of PR_3.
func setUp(t *testing.T, s *Store) (running, preparing, queued review.Review) {
	ctx := t.Context()
	if err := s.ReplaceQueue(ctx, []github.PullRequest{pr("PR_1", 0), pr("PR_2", 0), pr("PR_3", 0), pr("PR_4", 0)}); err != nil {
		t.Fatal(err)
	}
	for i, r := range []*review.Review{&running, &preparing, &queued} {
		req := request
		req.PRID = []string{"PR_1", "PR_2", "PR_3"}[i]
		var err error
		if *r, err = s.CreateReview(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, _, err := s.ClaimReview(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.StartReview(ctx, running.ID, "prompt"); err != nil {
		t.Fatal(err)
	}
	return running, preparing, queued
}

// statuses returns each review's status, with "!" when a cancel was requested.
func statuses(t *testing.T, s *Store) map[int64]string {
	list, err := s.Reviews(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	m := map[int64]string{}
	for _, r := range list {
		m[r.ID] = r.Status
		if r.CancelRequested {
			m[r.ID] += "!"
		}
	}
	return m
}

// A change and its event are saved together: if the event fails, the change is undone, so the
// caller's error is true and the pages can't miss a change.
func TestReviewChangeNeedsItsEvent(t *testing.T) {
	s := newStore(t)
	ctx := t.Context()
	running, preparing, queued := setUp(t, s)
	want := statuses(t, s)
	failEvents(t)

	if _, err := s.CreateReview(ctx, review.Request{PRID: "PR_4", Model: "sonnet", Effort: "high", TokenCap: 1000}); err == nil {
		t.Error("CreateReview() succeeded without its event")
	}
	if _, _, err := s.ClaimReview(ctx); err == nil {
		t.Error("ClaimReview() succeeded without its event")
	}
	if _, err := s.StartReview(ctx, preparing.ID, "prompt"); err == nil {
		t.Error("StartReview() succeeded without its event")
	}
	if _, err := s.CancelReview(ctx, queued.ID); err == nil {
		t.Error("CancelReview() succeeded without its event")
	}
	if err := s.FinishReview(ctx, running.ID, review.Outcome{Status: review.StatusDone}); err == nil || errors.Is(err, review.ErrNotActive) {
		t.Errorf("FinishReview() = %v, want the event's error", err)
	}
	if got := statuses(t, s); !maps.Equal(got, want) {
		t.Errorf("statuses = %v, want them unchanged: %v", got, want)
	}
}

// After an api restart, the reviews it was preparing are failed: nobody prepares them. The
// running ones are the worker's, so they're kept, for the broker to adopt.
func TestFailInterruptedReviews(t *testing.T) {
	s := newStore(t)
	running, preparing, queued := setUp(t, s)
	if err := s.FailInterruptedReviews(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{running.ID: review.StatusRunning, preparing.ID: review.StatusFailed, queued.ID: review.StatusQueued}
	if got := statuses(t, s); !maps.Equal(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
	if list, _ := s.Reviews(t.Context(), false); list[1].Error != "interrupted: the api restarted" || list[1].FinishedAt == nil {
		t.Errorf("review = %+v, want the reason", list[1])
	}
	if ids, err := s.RunningReviews(t.Context()); err != nil || !slices.Equal(ids, []int64{running.ID}) {
		t.Errorf("RunningReviews() = %v, %v; want [%d]", ids, err, running.ID)
	}
}

// claude's output and file names in the PR can hold bytes Postgres refuses in text.
func TestFinishReviewCleansText(t *testing.T) {
	s := newStore(t)
	running, _, _ := setUp(t, s)
	o := review.Outcome{Status: review.StatusFailed, Result: "a\x00b", Error: "create \xff\xfe.txt: file name too long"}
	if err := s.FinishReview(t.Context(), running.ID, o); err != nil {
		t.Fatalf("FinishReview() = %v, want the outcome stored", err)
	}
	list, _ := s.Reviews(t.Context(), false)
	if got := list[len(list)-1]; got.Result != "ab" || got.Error != "create \uFFFD.txt: file name too long" {
		t.Errorf("stored result %q, error %q", got.Result, got.Error)
	}
}
