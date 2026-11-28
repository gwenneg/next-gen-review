package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/github"
)

type fakeSource struct {
	queue []github.PullRequest
	err   error
}

func (f *fakeSource) Queue(context.Context, []string) ([]github.PullRequest, error) {
	return f.queue, f.err
}

type fakeStore struct {
	replaces, events int
	replaceErr       error
}

func (f *fakeStore) WatchedRepos(context.Context) ([]string, error) { return nil, nil }
func (f *fakeStore) ReplaceQueue(context.Context, []github.PullRequest) error {
	f.replaces++
	return f.replaceErr
}
func (f *fakeStore) AddEvent(context.Context, string) error {
	f.events++
	return nil
}

// Each step polls once. The queue is only saved when it changes, and the page gets an event
// when the queue changes (ReplaceQueue sends it) or when a poll error appears or goes away.
func TestPoll(t *testing.T) {
	a := []github.PullRequest{{ID: "a"}}
	b := []github.PullRequest{{ID: "b"}}
	c := []github.PullRequest{{ID: "c"}}
	boom := errors.New("GitHub returned 502")
	dbDown := errors.New("conn closed")
	steps := []struct {
		name       string
		queue      []github.PullRequest
		err        error
		replaceErr error
		replaces   int // ReplaceQueue calls so far
		events     int // AddEvent calls so far
		status     string
	}{
		{"first poll saves the queue", a, nil, nil, 1, 0, ""},
		{"same queue: nothing saved", a, nil, nil, 1, 0, ""},
		{"error: queue kept, page told", nil, boom, nil, 1, 1, boom.Error()},
		{"same error: no new event", nil, boom, nil, 1, 1, boom.Error()},
		{"recovered, same queue: page told", a, nil, nil, 1, 2, ""},
		{"new queue saved", b, nil, nil, 2, 2, ""},
		{"save fails: page told", c, nil, dbDown, 3, 3, "save queue: conn closed"},
		{"save fails again: retried, no new event", c, nil, dbDown, 4, 3, "save queue: conn closed"},
	}
	src, st := &fakeSource{}, &fakeStore{}
	p := NewPoller(src, st, 0)
	for _, s := range steps {
		src.queue, src.err, st.replaceErr = s.queue, s.err, s.replaceErr
		_ = p.Poll(context.Background())
		if st.replaces != s.replaces || st.events != s.events || p.Status().Error != s.status {
			t.Fatalf("%s: replaces=%d events=%d error=%q, want %d %d %q",
				s.name, st.replaces, st.events, p.Status().Error, s.replaces, s.events, s.status)
		}
	}
	if p.Status().RefreshedAt == nil {
		t.Error("RefreshedAt not set after a successful poll")
	}
}

// pollSource tells the test about each poll.
type pollSource struct{ polls chan struct{} }

func (p pollSource) Queue(context.Context, []string) ([]github.PullRequest, error) {
	p.polls <- struct{}{}
	return nil, nil
}

// Run polls at once, then again on Wake without waiting for the interval, until ctx ends.
func TestRun(t *testing.T) {
	src := pollSource{make(chan struct{})}
	p := NewPoller(src, &fakeStore{}, time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	for _, step := range []string{"at start", "after Wake"} {
		select {
		case <-src.polls:
		case <-time.After(2 * time.Second):
			t.Fatalf("no poll %s", step)
		}
		p.Wake()
		p.Wake() // a second wake doesn't block
	}
	cancel()
	go func() { // the poll the last wakes started may still be running
		for range src.polls {
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop with its context")
	}
}
