// Package github reads pull requests from GitHub with go-gh, the library the gh CLI uses.
package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/hashicorp/go-retryablehttp"
)

// Client calls the GitHub GraphQL and REST APIs with a token.
type Client struct {
	token string
	gql   *api.GraphQLClient
	rest  *api.RESTClient
}

// NewClient returns a client for github.com. An empty token is allowed: every call then fails
// with an error saying no token is set.
func NewClient(token string) (*Client, error) {
	return newClient(token, nil)
}

// retryWaitMin is the first wait before retrying a failed GitHub call. Tests shorten it.
var retryWaitMin = time.Second

// newClient lets tests send the requests to a fake server. GitHub's temporary errors (5xx, 429,
// network errors) are retried by go-retryablehttp.
func newClient(token string, transport http.RoundTripper) (*Client, error) {
	retry := retryablehttp.NewClient()
	retry.RetryMax = 3
	retry.RetryWaitMin, retry.RetryWaitMax = retryWaitMin, 10*time.Second
	retry.Logger = slog.Default()
	if transport != nil {
		retry.HTTPClient.Transport = transport
	}
	opts := api.ClientOptions{Host: "github.com", AuthToken: token, Timeout: 30 * time.Second,
		Transport: &retryablehttp.RoundTripper{Client: retry}}
	if token == "" {
		opts.AuthToken = "unset" // go-gh would otherwise look for gh's own config
	}
	gql, err := api.NewGraphQLClient(opts)
	if err != nil {
		return nil, err
	}
	rest, err := api.NewRESTClient(opts)
	if err != nil {
		return nil, err
	}
	return &Client{token: token, gql: gql, rest: rest}, nil
}

var errNoToken = errors.New("no GitHub token: set GH_TOKEN or GITHUB_TOKEN")

// Login returns the login of the token's user.
func (c *Client) Login(ctx context.Context) (string, error) {
	if c.token == "" {
		return "", errNoToken
	}
	var q struct{ Viewer struct{ Login string } }
	if err := c.gql.QueryWithContext(ctx, "Viewer", &q, nil); err != nil {
		return "", err
	}
	return q.Viewer.Login, nil
}

// InvalidRepoError means the repo can't be watched: bad name, missing, private or archived.
// Other errors from PublicRepo are GitHub or token problems.
type InvalidRepoError struct{ Msg string }

func (e InvalidRepoError) Error() string { return e.Msg }

var repoName = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)

// PublicRepo returns the repository name as GitHub spells it ("owner/name"). It returns an error
// if the repository is private, archived, missing, or not visible with the token.
func (c *Client) PublicRepo(ctx context.Context, fullName string) (string, error) {
	if !repoName.MatchString(fullName) {
		return "", InvalidRepoError{fmt.Sprintf("%q is not in the owner/name format", fullName)}
	}
	if c.token == "" {
		return "", errNoToken
	}
	var repo struct {
		FullName string `json:"full_name"`
		Private  bool   `json:"private"`
		Archived bool   `json:"archived"`
	}
	err := c.rest.DoWithContext(ctx, http.MethodGet, "repos/"+fullName, nil, &repo)
	if httpErr, ok := errors.AsType[*api.HTTPError](err); ok && httpErr.StatusCode == http.StatusNotFound {
		return "", InvalidRepoError{fullName + ": repository not found"}
	}
	switch {
	case err != nil:
		return "", err
	case repo.Private:
		return "", InvalidRepoError{repo.FullName + " is private: only public repos are supported"}
	case repo.Archived:
		return "", InvalidRepoError{repo.FullName + " is archived"}
	default:
		return repo.FullName, nil
	}
}
