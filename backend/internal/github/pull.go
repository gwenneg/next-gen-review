package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// maxDiff is how much of the diff goes in the prompt. Claude reads the files for the rest.
const maxDiff = 200 << 10

// PullRequest returns the PR's title, description and diff at the head commit sha, for the review
// prompt. The diff matches the code at sha even if the author pushed since.
func (c *Client) PullRequest(ctx context.Context, repo string, number int, sha string) (review.PR, error) {
	if c.token == "" {
		return review.PR{}, errNoToken
	}
	path := fmt.Sprintf("repos/%s/pulls/%d", repo, number)
	var pr struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  struct {
			SHA  string `json:"sha"`
			Repo *struct {
				FullName string `json:"full_name"`
			} `json:"repo"` // nil when the fork was deleted
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
	}
	if err := c.rest.DoWithContext(ctx, http.MethodGet, path, nil, &pr); err != nil {
		return review.PR{}, fmt.Errorf("get %s#%d: %w", repo, number, err)
	}
	// Comparing the base branch with sha gives the same diff as the PR's own, at sha.
	// owner:repo:sha also finds commits that are only in a fork.
	compare := ""
	if pr.Head.Repo != nil {
		compare = fmt.Sprintf("repos/%s/compare/%s...%s:%s", repo, pr.Base.Ref,
			strings.Replace(pr.Head.Repo.FullName, "/", ":", 1), sha)
	}
	// The PR's own diff follows its latest commit: when the author pushed since sha, compare.
	diffPath := path
	if pr.Head.SHA != sha {
		if compare == "" {
			return review.PR{}, fmt.Errorf("%s#%d changed since the queue was checked and its fork is gone: refresh the queue", repo, number)
		}
		diffPath = compare
	}
	diff, truncated, err := c.read(ctx, c.diff, diffPath, maxDiff)
	// GitHub refuses the PR's diff past 300 files with 406, but compare still gives it.
	var httpErr *api.HTTPError
	if errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotAcceptable && diffPath == path && compare != "" {
		diff, truncated, err = c.read(ctx, c.diff, compare, maxDiff)
	}
	if err != nil {
		return review.PR{}, fmt.Errorf("get the diff of %s#%d: %w", repo, number, err)
	}
	return review.PR{Repo: repo, Number: number, HeadSHA: sha, Title: pr.Title, Body: pr.Body,
		Diff: string(diff), DiffTruncated: truncated}, nil
}

// Tarball returns GitHub's gzipped tarball of the repo at a commit. The caller closes it.
func (c *Client) Tarball(ctx context.Context, repo, sha string) (io.ReadCloser, error) {
	if c.token == "" {
		return nil, errNoToken
	}
	resp, err := c.tarball.RequestWithContext(ctx, http.MethodGet, fmt.Sprintf("repos/%s/tarball/%s", repo, sha), nil)
	if err != nil {
		return nil, fmt.Errorf("download %s at %s: %w", repo, sha, err)
	}
	return resp.Body, nil
}

// read returns at most limit bytes of a raw response, and whether there was more.
func (c *Client) read(ctx context.Context, client interface {
	RequestWithContext(context.Context, string, string, io.Reader) (*http.Response, error)
}, path string, limit int64) ([]byte, bool, error) {
	resp, err := client.RequestWithContext(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limit {
		return body[:limit], true, nil
	}
	return body, false, nil
}
