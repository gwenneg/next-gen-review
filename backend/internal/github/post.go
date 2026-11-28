package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// Poster posts reviews to GitHub with its own token (GH_REVIEW_TOKEN), which can write PR reviews.
// The queue's token stays read-only. Its calls aren't retried: a second try of a POST that GitHub
// received would post the review twice.
type Poster struct {
	rest *api.RESTClient
}

// NewPoster returns a poster for github.com.
func NewPoster(token string) (*Poster, error) {
	return newPoster(token, nil)
}

func newPoster(token string, transport http.RoundTripper) (*Poster, error) {
	rest, err := api.NewRESTClient(api.ClientOptions{Host: "github.com", AuthToken: token, Timeout: requestTimeout,
		Transport: transport})
	if err != nil {
		return nil, err
	}
	return &Poster{rest: rest}, nil
}

// Login returns the login of the poster's token, which the reviews are posted as.
func (p *Poster) Login(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	err := p.rest.DoWithContext(ctx, http.MethodGet, "user", nil, &u)
	return u.Login, err
}

// ErrRefused means GitHub answered the post with a refusal: the review isn't on GitHub. Other errors,
// such as a timeout, a dropped connection or a server error, leave it unknown.
var ErrRefused = errors.New("GitHub refused the review")

// PostReview posts one review on the PR, at commit, with the inline comments, and returns its URL.
// event is one of review.Events.
func (p *Poster) PostReview(ctx context.Context, repo string, number int, commit, event, body string, comments []review.Comment) (string, error) {
	type comment struct {
		Path      string `json:"path"`
		Body      string `json:"body"`
		Line      int    `json:"line"`
		Side      string `json:"side"`
		StartLine int    `json:"start_line,omitempty"`
		StartSide string `json:"start_side,omitempty"`
	}
	in := struct {
		CommitID string    `json:"commit_id"`
		Body     string    `json:"body"`
		Event    string    `json:"event"`
		Comments []comment `json:"comments"`
	}{CommitID: commit, Body: body, Event: event, Comments: []comment{}}
	for _, c := range comments {
		out := comment{Path: c.Path, Body: c.Body, Line: c.Line, Side: c.Side}
		if c.StartLine != 0 && c.StartLine != c.Line {
			out.StartLine, out.StartSide = c.StartLine, c.Side
		}
		in.Comments = append(in.Comments, out)
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	var posted struct {
		HTMLURL string `json:"html_url"`
	}
	path := fmt.Sprintf("repos/%s/pulls/%d/reviews", repo, number)
	if err := p.rest.DoWithContext(ctx, http.MethodPost, path, bytes.NewReader(payload), &posted); err != nil {
		// A 4xx is a refusal, but GitHub may have the review after a timeout (408) or a 429.
		if httpErr, ok := errors.AsType[*api.HTTPError](err); ok && httpErr.StatusCode >= 400 && httpErr.StatusCode < 500 &&
			httpErr.StatusCode != http.StatusRequestTimeout && httpErr.StatusCode != http.StatusTooManyRequests {
			return "", fmt.Errorf("%w: %w", ErrRefused, err)
		}
		return "", err
	}
	if posted.HTMLURL == "" {
		return "", errors.New("GitHub's answer has no link to the review")
	}
	return posted.HTMLURL, nil
}

// FindReview returns the link of a review author posted on the PR, at commit, with this main
// comment, if any: a post whose answer was lost may have reached GitHub.
func (p *Poster) FindReview(ctx context.Context, repo string, number int, commit, author, body string) (string, bool, error) {
	for page := 1; ; page++ {
		var reviews []struct {
			HTMLURL  string `json:"html_url"`
			CommitID string `json:"commit_id"`
			Body     string `json:"body"`
			User     struct {
				Login string `json:"login"`
			} `json:"user"`
		}
		path := fmt.Sprintf("repos/%s/pulls/%d/reviews?per_page=100&page=%d", repo, number, page)
		if err := p.rest.DoWithContext(ctx, http.MethodGet, path, nil, &reviews); err != nil {
			return "", false, err
		}
		for _, r := range reviews {
			if r.CommitID == commit && strings.EqualFold(r.User.Login, author) && strings.TrimSpace(r.Body) == strings.TrimSpace(body) {
				return r.HTMLURL, true, nil
			}
		}
		if len(reviews) < 100 {
			return "", false, nil
		}
	}
}
