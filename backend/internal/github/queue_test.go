package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeGitHub answers the viewer and search GraphQL queries, the latter by query string (with an
// optional second page), and REST repository lookups by "owner/name".
type fakeGitHub struct {
	searches map[string][][]map[string]any // search query -> pages of nodes
	repos    map[string]map[string]any     // "owner/name" -> REST repository
	failures int                           // the first requests answered with 502
	calls    *int
}

func (f fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	*f.calls++
	if *f.calls <= f.failures {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	if name, ok := strings.CutPrefix(r.URL.Path, "/repos/"); ok {
		repo, found := f.repos[name]
		if !found {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(repo)
		return
	}
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var data any
	switch {
	case strings.HasPrefix(req.Query, "query Viewer"):
		data = map[string]any{"viewer": map[string]any{"login": "me"}}
	case strings.HasPrefix(req.Query, "query Search"):
		data = f.searchPage(req.Variables)
	default:
		http.Error(w, "unexpected query: "+req.Query, http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func (f fakeGitHub) searchPage(vars map[string]any) any {
	pages := f.searches[vars["q"].(string)]
	page := 0
	if vars["after"] != nil {
		page = 1
	}
	var nodes []map[string]any
	if page < len(pages) {
		nodes = pages[page]
	}
	return map[string]any{"search": map[string]any{
		"pageInfo": map[string]any{"hasNextPage": page+1 < len(pages), "endCursor": "c1"},
		"nodes":    nodes,
	}}
}

// toServer sends every request to the fake server instead of api.github.com.
type toServer struct{ url *url.URL }

func (t toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme, r.URL.Host = t.url.Scheme, t.url.Host
	return http.DefaultTransport.RoundTrip(r)
}

func newTestClient(t *testing.T, f fakeGitHub) *Client {
	f.calls = new(int)
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	c, err := newClient("token", toServer{u})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// pr builds a search node. Options set the fields a test case needs.
func pr(id string, created string, opts ...func(map[string]any)) map[string]any {
	n := map[string]any{
		"id": id, "number": 1, "title": id, "url": "https://github.com/o/r/pull/1", "createdAt": created,
		"updatedAt": created, "headRefOid": "head", "author": map[string]any{"login": "author"},
		"repository": map[string]any{"nameWithOwner": "o/r", "isPrivate": false},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

func requestedFrom(reviewers ...map[string]any) func(map[string]any) {
	return func(n map[string]any) {
		nodes := make([]any, len(reviewers))
		for i, r := range reviewers {
			nodes[i] = map[string]any{"requestedReviewer": r}
		}
		n["reviewRequests"] = map[string]any{"nodes": nodes}
	}
}

func myReviewOn(commit any) func(map[string]any) {
	return func(n map[string]any) {
		var c any
		if commit != nil {
			c = map[string]any{"oid": commit}
		}
		n["reviews"] = map[string]any{"nodes": []any{map[string]any{"commit": c}}}
	}
}

func private(n map[string]any) { n["repository"].(map[string]any)["isPrivate"] = true }

const (
	qRequested = base + "review-requested:@me"
	qReviewed  = base + "reviewed-by:@me -author:@me"
	qWatched   = base + "draft:false -author:@me -reviewed-by:@me repo:o/r"
)

func TestQueue(t *testing.T) {
	user := map[string]any{"__typename": "User", "login": "me"}
	team := map[string]any{"__typename": "Team", "combinedSlug": "org/team"}

	f := fakeGitHub{searches: map[string][][]map[string]any{
		qRequested: {
			{pr("direct", "2026-01-03T00:00:00Z", requestedFrom(team, user))},
			{pr("team", "2026-01-02T00:00:00Z", requestedFrom(team))}, // second page
		},
		qReviewed: {{
			pr("up-to-date", "2026-01-04T00:00:00Z", myReviewOn("head")),
			pr("updated", "2026-01-05T00:00:00Z", myReviewOn("old")),
			pr("force-pushed", "2026-01-06T00:00:00Z", myReviewOn(nil)),
		}},
		qWatched: {{
			pr("team", "2026-01-02T00:00:00Z"),
			pr("watched", "2026-01-01T00:00:00Z"),
			pr("tie", "2026-01-01T00:00:00Z"), // same time as "watched": ID breaks the tie
			pr("private", "2026-01-01T00:00:00Z", private),
		}},
	}}

	queue, err := newTestClient(t, f).Queue(context.Background(), []string{"o/r"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]Reason{}
	var order []string
	for _, p := range queue {
		got[p.ID] = p.Reasons
		order = append(order, p.ID)
	}
	want := map[string][]Reason{
		"direct":       {{Kind: ReasonRequested}},
		"team":         {{Kind: ReasonRequested, ViaTeam: true, Teams: []string{"org/team"}}, {Kind: ReasonWatched}},
		"updated":      {{Kind: ReasonNewCommits}},
		"force-pushed": {{Kind: ReasonNewCommits}},
		"watched":      {{Kind: ReasonWatched}},
		"tie":          {{Kind: ReasonWatched}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("reasons = %+v\nwant %+v", got, want)
	}
	if wantOrder := []string{"tie", "watched", "team", "direct", "updated", "force-pushed"}; !reflect.DeepEqual(order, wantOrder) {
		t.Errorf("order = %v, want oldest first %v", order, wantOrder)
	}
}

// Watched repos share searches, 20 per search.
func TestQueueWatchedChunks(t *testing.T) {
	watched := make([]string, 21)
	for i := range watched {
		watched[i] = fmt.Sprintf("o/r%d", i)
	}
	search := func(repos []string) string {
		return base + "draft:false -author:@me -reviewed-by:@me repo:" + strings.Join(repos, " repo:")
	}
	f := fakeGitHub{searches: map[string][][]map[string]any{
		search(watched[:20]): {{pr("first", "2026-01-01T00:00:00Z")}},
		search(watched[20:]): {{pr("second", "2026-01-02T00:00:00Z")}},
	}}
	queue, err := newTestClient(t, f).Queue(context.Background(), watched)
	if err != nil {
		t.Fatal(err)
	}
	if len(queue) != 2 || queue[0].ID != "first" || queue[1].ID != "second" {
		t.Errorf("queue = %+v, want the PRs from both searches", queue)
	}
}

// A temporary GitHub error is retried instead of failing the poll.
func TestRetry(t *testing.T) {
	f := fakeGitHub{failures: 1}
	retryWaitMin = time.Millisecond
	if _, err := newTestClient(t, f).Login(context.Background()); err != nil {
		t.Errorf("Login after one 502 = %v, want a retry to succeed", err)
	}
}

func TestPublicRepo(t *testing.T) {
	f := fakeGitHub{repos: map[string]map[string]any{
		"o/public":   {"full_name": "Owner/Public"},
		"o/private":  {"full_name": "o/private", "private": true},
		"o/archived": {"full_name": "o/archived", "archived": true},
	}}
	c := newTestClient(t, f)
	tests := []struct {
		in, want, err string
	}{
		{"o/public", "Owner/Public", ""},
		{"o/private", "", "is private"},
		{"o/archived", "", "is archived"},
		{"o/missing", "", "not found"},
		{"no-slash", "", "owner/name format"},
		{"o/r/extra", "", "owner/name format"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			got, err := c.PublicRepo(ctx, tt.in)
			if tt.err == "" && (err != nil || got != tt.want) {
				t.Fatalf("got %q, %v; want %q", got, err, tt.want)
			}
			if tt.err != "" && (err == nil || !strings.Contains(err.Error(), tt.err)) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.err)
			}
			if _, invalid := errors.AsType[InvalidRepoError](err); tt.err != "" && !invalid {
				t.Errorf("err = %T, want InvalidRepoError so the api answers 400", err)
			}
		})
	}
}
