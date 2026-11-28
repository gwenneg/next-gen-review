package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// The review goes out as GitHub's API takes it, once even when GitHub fails.
func TestPostReview(t *testing.T) {
	var got map[string]any
	posts := 0
	status := http.StatusOK
	noLink := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/user":
			_, _ = w.Write([]byte(`{"login":"maintainer"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls/7/reviews":
			posts++
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.WriteHeader(status)
			if noLink {
				_, _ = w.Write([]byte(`{}`))
			} else {
				_, _ = w.Write([]byte(`{"html_url":"https://github.com/o/r/pull/7#pullrequestreview-1"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p, err := newPoster("token", toServer{u})
	if err != nil {
		t.Fatal(err)
	}
	if login, err := p.Login(t.Context()); err != nil || login != "maintainer" {
		t.Errorf("Login() = %q, %v", login, err)
	}

	comments := []review.Comment{
		{ID: 1, Path: "a.go", Line: 4, Side: review.SideRight, Body: "one line"},
		{ID: 2, Path: "a.go", StartLine: 2, Line: 5, Side: review.SideLeft, Body: "a range"},
	}
	link, err := p.PostReview(t.Context(), "o/r", 7, "abc", "APPROVE", "main", comments)
	if err != nil || link != "https://github.com/o/r/pull/7#pullrequestreview-1" {
		t.Fatalf("PostReview() = %q, %v", link, err)
	}
	want := map[string]any{"commit_id": "abc", "body": "main", "event": "APPROVE", "comments": []any{
		map[string]any{"path": "a.go", "line": 4.0, "side": "RIGHT", "body": "one line"},
		map[string]any{"path": "a.go", "line": 5.0, "side": "LEFT", "start_line": 2.0, "start_side": "LEFT", "body": "a range"},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("posted %v\nwant %v", got, want)
	}

	// Only a 4xx says the review isn't on GitHub. After a timeout or a server error, it may be.
	for code, refused := range map[int]bool{http.StatusUnprocessableEntity: true, http.StatusBadGateway: false,
		http.StatusRequestTimeout: false, http.StatusTooManyRequests: false} {
		status = code
		if _, err := p.PostReview(t.Context(), "o/r", 7, "abc", "COMMENT", "main", nil); err == nil || errors.Is(err, ErrRefused) != refused {
			t.Errorf("PostReview() answered %d = %v; want refused: %v", code, err, refused)
		}
	}
	if posts != 5 {
		t.Errorf("GitHub got %d posts, want one per call: a retried post would post twice", posts)
	}
	status = http.StatusOK
	noLink = true
	if _, err := p.PostReview(t.Context(), "o/r", 7, "abc", "COMMENT", "main", nil); err == nil || errors.Is(err, ErrRefused) {
		t.Errorf("PostReview() answered without a link = %v, want an error that isn't a refusal", err)
	}
	if got["comments"] == nil {
		t.Error("a review without inline comments sends no comments list: send an empty one")
	}
}

// FindReview finds the review a lost answer was about: by the token's user, at the commit, with
// the same main comment, on any page of the PR's reviews.
func TestFindReview(t *testing.T) {
	review := func(id int, login, commit, body string) map[string]any {
		return map[string]any{"html_url": fmt.Sprintf("https://github.com/o/r/pull/7#pullrequestreview-%d", id),
			"commit_id": commit, "body": body, "user": map[string]any{"login": login}}
	}
	var page1 []map[string]any
	for i := range 100 {
		page1 = append(page1, review(i, "someone", "abc", "main"))
	}
	pages := map[string][]map[string]any{
		"1": append(page1[:99], review(99, "maintainer", "old", "main")), // the same text on an older commit
		"2": {review(200, "maintainer", "abc", "other"), review(201, "Maintainer", "abc", "main\n")},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(pages[r.URL.Query().Get("page")])
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	p, _ := newPoster("token", toServer{u})

	link, found, err := p.FindReview(t.Context(), "o/r", 7, "abc", "maintainer", "main")
	if err != nil || !found || link != "https://github.com/o/r/pull/7#pullrequestreview-201" {
		t.Errorf("FindReview() = %q, %v, %v; want review 201", link, found, err)
	}
	if _, found, err := p.FindReview(t.Context(), "o/r", 7, "abc", "maintainer", "never posted"); found || err != nil {
		t.Errorf("FindReview(not posted) = %v, %v", found, err)
	}
}
