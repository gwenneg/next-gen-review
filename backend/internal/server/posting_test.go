package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// fakeDrafts has review 1, ended, on o/r#7 at commit "abc", with one inline comment and one off
// the diff. Review 2 is running, review 3 is on a repo that turned private.
type fakeDrafts struct {
	mu        sync.Mutex
	posting   review.Posting
	posted    []string // url, event
	markErr   error
	edits     []string
	lastAdded review.Comment
}

func (f *fakeDrafts) Review(_ context.Context, id int64) (review.Review, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := review.Review{ID: id, Repo: "o/r", Number: 7, HeadSHA: "abc", Status: review.StatusDone}
	switch id {
	case 2:
		r.Status = review.StatusRunning
	case 3:
		r.Repo = "o/private"
	case 99:
		return r, false, nil
	}
	return r, true, nil
}
func (f *fakeDrafts) Posting(_ context.Context, id int64) (review.Posting, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == 99 {
		return review.Posting{}, review.ErrUnknownReview
	}
	return f.posting, nil
}
func (f *fakeDrafts) SetMainComment(_ context.Context, _ int64, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.posting.Posted { // like the store
		return review.ErrPosted
	}
	f.edits = append(f.edits, "main:"+body)
	return nil
}
func (f *fakeDrafts) AddComment(_ context.Context, _ int64, c review.Comment) (review.Comment, error) {
	f.lastAdded = c
	return c, nil
}
func (f *fakeDrafts) SetComment(_ context.Context, _ int64, cid int64, body string) error {
	if cid == 99 {
		return review.ErrUnknownComment
	}
	f.edits = append(f.edits, "comment:"+body)
	return nil
}
func (f *fakeDrafts) DeleteComment(context.Context, int64, int64) error { return nil }
func (f *fakeDrafts) MarkPosted(_ context.Context, _ int64, url, event string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.markErr != nil {
		return f.markErr
	}
	f.posted = []string{url, event}
	f.posting.Posted, f.posting.PostedURL, f.posting.PostedEvent = true, url, event
	return nil
}

type fakePoster struct {
	mu                  sync.Mutex
	delay               time.Duration // a slow GitHub
	posts               int
	err                 error
	repo, commit, event string
	number              int
	body                string
	comments            []review.Comment
	earlier             string // the link of a review FindReview finds
	findErr             error
	found               []string // what FindReview looked for: author, body
}

func (f *fakePoster) FindReview(_ context.Context, _ string, _ int, _, author, body string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.found = []string{author, body}
	return f.earlier, f.earlier != "", f.findErr
}

func (f *fakePoster) PostReview(_ context.Context, repo string, number int, commit, event, body string, comments []review.Comment) (string, error) {
	time.Sleep(f.delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	f.repo, f.number, f.commit, f.event, f.body, f.comments = repo, number, commit, event, body, comments
	return "https://github.com/o/r/pull/7#pullrequestreview-1", f.err
}

func newPostingDeps() (Deps, *fakeDrafts, *fakePoster) {
	d := newDeps()
	drafts := &fakeDrafts{posting: review.Posting{Main: "Please fix the retry.", Comments: []review.Comment{
		{ID: 1, Path: "a.go", Line: 4, Side: review.SideRight, Body: "[High] Retries forever.", InDiff: true},
		{ID: 2, Path: "b.go", Line: 9, Side: review.SideRight, Body: "[Low] Rename.", InDiff: false},
	}}}
	poster := &fakePoster{}
	d.Drafts, d.Poster, d.PosterLogin = drafts, poster, "maintainer"
	return d, drafts, poster
}

func send(t *testing.T, d Deps, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	New(d).ServeHTTP(rec, req)
	return rec
}

// Posting sends one review at the reviewed commit, with the maintainer's verdict: inline comments
// where GitHub takes them, the others in the main comment, and each one labeled as drafted with AI.
func TestPost(t *testing.T) {
	d, drafts, poster := newPostingDeps()
	rec := send(t, d, http.MethodPost, "/api/reviews/1/posting/post", `{"event":"REQUEST_CHANGES"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	if poster.repo != "o/r" || poster.number != 7 || poster.commit != "abc" || poster.event != "REQUEST_CHANGES" {
		t.Errorf("posted to %s#%d at %s as %s", poster.repo, poster.number, poster.commit, poster.event)
	}
	if want := review.MainBody("Please fix the retry.", drafts.posting.Comments[1:]); poster.body != want {
		t.Errorf("main comment = %q\nwant %q", poster.body, want)
	}
	if len(poster.comments) != 1 || poster.comments[0].Body != review.CommentBody("[High] Retries forever.") {
		t.Errorf("inline comments = %+v, want the one in the diff, labeled", poster.comments)
	}
	if poster.found[0] != "maintainer" || poster.found[1] != poster.body {
		t.Errorf("looked for an earlier post by %q with %q", poster.found[0], poster.found[1])
	}
	var got postingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || !got.Posted || got.PostedURL == "" || got.PostedEvent != "REQUEST_CHANGES" || got.PostsAs != "maintainer" {
		t.Errorf("response = %s", rec.Body)
	}
	// Posted is posted: a second click doesn't post again.
	poster.event = ""
	if rec := send(t, d, http.MethodPost, "/api/reviews/1/posting/post", `{"event":"COMMENT"}`); rec.Code != http.StatusConflict || poster.event != "" {
		t.Errorf("second post: status %d, posted as %q", rec.Code, poster.event)
	}
}

// A double click, or two tabs, posts once.
func TestPostOnce(t *testing.T) {
	d, _, poster := newPostingDeps()
	poster.delay = 100 * time.Millisecond
	h := New(d)
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Go(func() {
			req := httptest.NewRequest(http.MethodPost, "/api/reviews/1/posting/post", strings.NewReader(`{"event":"COMMENT"}`))
			req.Host = "127.0.0.1:8080"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes[i] = rec.Code
		})
	}
	wg.Wait()
	if poster.posts != 1 || codes[0]+codes[1] != http.StatusOK+http.StatusConflict {
		t.Errorf("GitHub got %d posts, answers %v; want one post, and a 409 for the other", poster.posts, codes)
	}
}

// A post whose answer was lost may be on GitHub: the next post records it instead of posting again.
func TestPostAfterLostAnswer(t *testing.T) {
	d, drafts, poster := newPostingDeps()
	poster.err = errors.New("Post: context deadline exceeded (Client.Timeout exceeded while awaiting headers)")
	rec := send(t, d, http.MethodPost, "/api/reviews/1/posting/post", `{"event":"APPROVE"}`)
	if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), "may be posted") || drafts.posted != nil {
		t.Fatalf("status = %d, body %s, recorded %v; want a 502 saying it may be posted", rec.Code, rec.Body, drafts.posted)
	}
	poster.err, poster.earlier = nil, "https://github.com/o/r/pull/7#pullrequestreview-9"
	rec = send(t, d, http.MethodPost, "/api/reviews/1/posting/post", `{"event":"APPROVE"}`)
	if rec.Code != http.StatusOK || poster.posts != 1 || drafts.posted[0] != poster.earlier {
		t.Errorf("status = %d, GitHub got %d posts, recorded %v; want the earlier review recorded, not a second post",
			rec.Code, poster.posts, drafts.posted)
	}
}

// An edit made while a post is on its way waits for it, then finds the comments posted: it
// would otherwise be stored but not posted.
func TestEditDuringPost(t *testing.T) {
	d, drafts, poster := newPostingDeps()
	poster.delay = 200 * time.Millisecond
	h := New(d)
	posted := make(chan int)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/api/reviews/1/posting/post", strings.NewReader(`{"event":"COMMENT"}`))
		req.Host = "127.0.0.1:8080"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		posted <- rec.Code
	}()
	time.Sleep(50 * time.Millisecond) // the post is on its way
	req := httptest.NewRequest(http.MethodPut, "/api/reviews/1/posting/main", strings.NewReader(`{"body":"Too late."}`))
	req.Host = "127.0.0.1:8080"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if code := <-posted; code != http.StatusOK || rec.Code != http.StatusConflict || len(drafts.edits) != 0 {
		t.Errorf("post: %d, edit: %d (%s), edits stored %v; want the edit refused as posted", code, rec.Code, rec.Body, drafts.edits)
	}
}

// A comment's limit leaves room for the AI label, in characters, not bytes.
func TestCommentLength(t *testing.T) {
	d, _, _ := newPostingDeps()
	for n, status := range map[int]int{review.MaxComment: http.StatusOK, review.MaxComment + 1: http.StatusBadRequest} {
		body, _ := json.Marshal(map[string]string{"body": strings.Repeat("é", n)})
		if rec := send(t, d, http.MethodPut, "/api/reviews/1/posting/comments/1", string(body)); rec.Code != status {
			t.Errorf("a comment of %d characters: status %d (%s), want %d", n, rec.Code, rec.Body, status)
		}
	}
}

func TestPostRefused(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		body   string
		setUp  func(d *Deps, drafts *fakeDrafts, poster *fakePoster)
		status int
		msg    string
	}{
		{"bad verdict", "/api/reviews/1/posting/post", `{"event":"MERGE"}`, nil, http.StatusBadRequest, "COMMENT, APPROVE or REQUEST_CHANGES"},
		{"posting off", "/api/reviews/1/posting/post", `{"event":"COMMENT"}`, func(d *Deps, _ *fakeDrafts, _ *fakePoster) { d.Poster = nil }, http.StatusConflict, "GH_REVIEW_TOKEN"},
		{"review running", "/api/reviews/2/posting/post", `{"event":"COMMENT"}`, nil, http.StatusConflict, "still active"},
		{"unknown review", "/api/reviews/99/posting/post", `{"event":"COMMENT"}`, nil, http.StatusNotFound, "no review"},
		{"repo turned private", "/api/reviews/3/posting/post", `{"event":"COMMENT"}`, nil, http.StatusBadGateway, "private"},
		{"GitHub refuses", "/api/reviews/1/posting/post", `{"event":"APPROVE"}`, func(_ *Deps, _ *fakeDrafts, p *fakePoster) {
			p.err = fmt.Errorf("%w: HTTP 422: Can not approve your own pull request", github.ErrRefused)
		}, http.StatusBadGateway, "GitHub refused the review: HTTP 422: Can not approve your own pull request"},
		{"earlier posts can't be checked", "/api/reviews/1/posting/post", `{"event":"COMMENT"}`, func(_ *Deps, _ *fakeDrafts, p *fakePoster) {
			p.findErr = errors.New("HTTP 502")
		}, http.StatusBadGateway, "look for an earlier post of this review: HTTP 502"},
		{"token's user unknown", "/api/reviews/1/posting/post", `{"event":"COMMENT"}`, func(d *Deps, _ *fakeDrafts, _ *fakePoster) {
			d.PosterLogin = ""
		}, http.StatusConflict, "who GH_REVIEW_TOKEN belongs to"},
		{"already posted", "/api/reviews/1/posting/post", `{"event":"COMMENT"}`, func(_ *Deps, f *fakeDrafts, _ *fakePoster) {
			f.posting.Posted = true // even if its link wasn't recorded
		}, http.StatusConflict, "already posted"},
		{"an inline comment too long", "/api/reviews/1/posting/post", `{"event":"COMMENT"}`, func(_ *Deps, f *fakeDrafts, _ *fakePoster) {
			f.posting.Comments[0].Body = strings.Repeat("a", review.MaxComment+1) // an AI draft isn't checked when stored
		}, http.StatusBadRequest, "the comment on a.go:4 is too long"},
		{"the main comment too long", "/api/reviews/1/posting/post", `{"event":"COMMENT"}`, func(_ *Deps, f *fakeDrafts, _ *fakePoster) {
			f.posting.Main = strings.Repeat("a", review.MaxBody-50)
		}, http.StatusBadRequest, "the main comment, with the comments outside the diff, has"},
		// GitHub has it: the page must say so, with its link, so it isn't posted twice.
		{"posted but not recorded", "/api/reviews/1/posting/post", `{"event":"COMMENT"}`, func(_ *Deps, f *fakeDrafts, _ *fakePoster) {
			f.markErr = errors.New("conn closed")
		}, http.StatusInternalServerError, "don't post it again (https://github.com/o/r/pull/7#pullrequestreview-1)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, drafts, poster := newPostingDeps()
			if tt.setUp != nil {
				tt.setUp(&d, drafts, poster)
			}
			rec := send(t, d, http.MethodPost, tt.path, tt.body)
			if rec.Code != tt.status || !strings.Contains(rec.Body.String(), tt.msg) {
				t.Errorf("status = %d, body %s; want %d with %q", rec.Code, rec.Body, tt.status, tt.msg)
			}
		})
	}
}

// The page reads and edits the comments; each answer is the comments as they are now.
func TestEditDrafts(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		status                   int
	}{
		{"read", http.MethodGet, "/api/reviews/1/posting", "", http.StatusOK},
		{"read an unknown review", http.MethodGet, "/api/reviews/99/posting", "", http.StatusNotFound},
		{"set the main comment", http.MethodPut, "/api/reviews/1/posting/main", `{"body":"New text."}`, http.StatusOK},
		{"main comment too long", http.MethodPut, "/api/reviews/1/posting/main", `{"body":"` + strings.Repeat("a", review.MaxComment+1) + `"}`, http.StatusBadRequest},
		{"set a comment", http.MethodPut, "/api/reviews/1/posting/comments/1", `{"body":"Better."}`, http.StatusOK},
		{"empty a comment", http.MethodPut, "/api/reviews/1/posting/comments/1", `{"body":""}`, http.StatusBadRequest},
		{"set an unknown comment", http.MethodPut, "/api/reviews/1/posting/comments/99", `{"body":"x"}`, http.StatusNotFound},
		{"delete a comment", http.MethodDelete, "/api/reviews/1/posting/comments/1", "", http.StatusOK},
		{"bad comment id", http.MethodDelete, "/api/reviews/1/posting/comments/x", "", http.StatusBadRequest},
		{"add a comment", http.MethodPost, "/api/reviews/1/posting/comments", `{"path":"a.go","line":5,"startLine":3,"side":"LEFT","body":"Mine."}`, http.StatusOK},
		{"add one without a line", http.MethodPost, "/api/reviews/1/posting/comments", `{"path":"a.go","side":"RIGHT","body":"x"}`, http.StatusBadRequest},
		{"add one on a bad side", http.MethodPost, "/api/reviews/1/posting/comments", `{"path":"a.go","line":5,"side":"UP","body":"x"}`, http.StatusBadRequest},
		{"add one with a backward range", http.MethodPost, "/api/reviews/1/posting/comments", `{"path":"a.go","line":5,"startLine":8,"side":"RIGHT","body":"x"}`, http.StatusBadRequest},
		{"add an empty one", http.MethodPost, "/api/reviews/1/posting/comments", `{"path":"a.go","line":5,"side":"RIGHT","body":""}`, http.StatusBadRequest},
		{"bad json", http.MethodPut, "/api/reviews/1/posting/main", `{`, http.StatusBadRequest},
		{"bad review id", http.MethodGet, "/api/reviews/x/posting", "", http.StatusBadRequest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, drafts, _ := newPostingDeps()
			rec := send(t, d, tt.method, tt.path, tt.body)
			if rec.Code != tt.status {
				t.Fatalf("status = %d, body %s; want %d", rec.Code, rec.Body, tt.status)
			}
			if tt.status == http.StatusOK && !strings.Contains(rec.Body.String(), `"main":"Please fix the retry."`) {
				t.Errorf("body = %s, want the comments", rec.Body)
			}
			if tt.name == "add a comment" && drafts.lastAdded != (review.Comment{Path: "a.go", Line: 5, StartLine: 3, Side: review.SideLeft, Body: "Mine."}) {
				t.Errorf("added %+v", drafts.lastAdded)
			}
		})
	}
	// From another site: refused like every other change.
	d, drafts, _ := newPostingDeps()
	req := httptest.NewRequest(http.MethodPut, "/api/reviews/1/posting/main", strings.NewReader(`{"body":"x"}`))
	req.Host = "127.0.0.1:8080"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://evil.example")
	rec := httptest.NewRecorder()
	New(d).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || len(drafts.edits) != 0 {
		t.Errorf("cross-site edit: status %d, edits %v", rec.Code, drafts.edits)
	}
}
