package github

import (
	"context"
	"slices"
	"strings"
	"time"

	graphql "github.com/cli/shurcooL-graphql"
)

// Reason kinds: why a pull request is waiting for the user.
const (
	ReasonRequested  = "requested"   // someone asked for the user's review, directly or through a team
	ReasonWatched    = "watched"     // in a watched repo, not a draft, and not reviewed by the user yet
	ReasonNewCommits = "new_commits" // reviewed by the user, then updated by the author
)

// Reason says why a pull request is in the queue.
type Reason struct {
	Kind string `json:"kind"`
	// ViaTeam is set when the user wasn't requested directly, only through a team.
	ViaTeam bool `json:"viaTeam,omitempty"`
	// Teams are all the teams requested on the PR ("org/slug"), when the user wasn't requested
	// directly. The user is in at least one of them, but which one isn't known: that needs read:org.
	Teams []string `json:"teams,omitempty"`
}

// PullRequest is one queue item.
type PullRequest struct {
	ID           string    `json:"id"`
	Repo         string    `json:"repo"`
	Number       int       `json:"number"`
	Title        string    `json:"title"`
	URL          string    `json:"url"`
	Author       string    `json:"author"`
	IsDraft      bool      `json:"isDraft"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Additions    int       `json:"additions"`
	Deletions    int       `json:"deletions"`
	ChangedFiles int       `json:"changedFiles"`
	HeadSHA      string    `json:"headSha"`
	CIState      string    `json:"ciState,omitempty"` // SUCCESS, FAILURE, PENDING, ERROR, EXPECTED, or empty without checks
	Reasons      []Reason  `json:"reasons"`
}

// watchedPerSearch is how many watched repos share one search. A test with 30 worked; 20 leaves a
// margin, and 20 repos with fewer than 50 open PRs each stay under GitHub's 1,000 results per search.
const watchedPerSearch = 20

// Every search is limited to public, non-archived repos, so private PRs never reach the queue.
const base = "is:pr is:open is:public archived:false "

// searchResult is the GraphQL query: go-gh builds the query from the struct and its tags.
type searchResult struct {
	Search struct {
		PageInfo struct {
			HasNextPage bool
			EndCursor   string
		}
		Nodes []struct {
			PR prNode `graphql:"... on PullRequest"`
		}
	} `graphql:"search(query: $q, type: ISSUE, first: 100, after: $after)"`
}

type prNode struct {
	ID           string
	Number       int
	Title        string
	URL          string
	IsDraft      bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Additions    int
	Deletions    int
	ChangedFiles int
	HeadRefOid   string
	Author       struct{ Login string }
	Repository   struct {
		NameWithOwner string
		IsPrivate     bool
	}
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup struct{ State string }
			}
		}
	} `graphql:"commits(last: 1)"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer struct {
				Typename string                        `graphql:"__typename"`
				User     struct{ Login string }        `graphql:"... on User"`
				Team     struct{ CombinedSlug string } `graphql:"... on Team"`
			}
		}
	} `graphql:"reviewRequests(first: 100)"`
	Reviews struct {
		Nodes []struct {
			Commit struct{ Oid string } // empty when the reviewed commit was force-pushed away
		}
	} `graphql:"reviews(author: $me, last: 1)"`
}

func (c *Client) search(ctx context.Context, q, me string) ([]prNode, error) {
	var all []prNode
	vars := map[string]any{"q": graphql.String(q), "me": graphql.String(me), "after": (*graphql.String)(nil)}
	for {
		var res searchResult
		if err := c.gql.QueryWithContext(ctx, "Search", &res, vars); err != nil {
			return nil, err
		}
		for _, n := range res.Search.Nodes {
			all = append(all, n.PR)
		}
		if !res.Search.PageInfo.HasNextPage {
			return all, nil
		}
		vars["after"] = graphql.NewString(graphql.String(res.Search.PageInfo.EndCursor))
	}
}

// Queue returns the pull requests waiting for the user, oldest first. A pull request that
// matches several searches appears once, with one reason per search.
func (c *Client) Queue(ctx context.Context, watched []string) ([]PullRequest, error) {
	me, err := c.Login(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]*PullRequest{}
	add := func(n prNode, r Reason) {
		if n.Repository.IsPrivate { // the searches exclude private repos; this is a second guard
			return
		}
		pr, ok := byID[n.ID]
		if !ok {
			pr = toPullRequest(n)
			byID[n.ID] = pr
		}
		pr.Reasons = append(pr.Reasons, r)
	}

	requested, err := c.search(ctx, base+"review-requested:@me", me)
	if err != nil {
		return nil, err
	}
	for _, n := range requested {
		add(n, requestReason(n, me))
	}

	reviewed, err := c.search(ctx, base+"reviewed-by:@me -author:@me", me)
	if err != nil {
		return nil, err
	}
	for _, n := range reviewed {
		if len(n.Reviews.Nodes) > 0 && n.Reviews.Nodes[0].Commit.Oid != n.HeadRefOid {
			add(n, Reason{Kind: ReasonNewCommits})
		}
	}

	for chunk := range slices.Chunk(watched, watchedPerSearch) {
		nodes, err := c.search(ctx, base+"draft:false -author:@me -reviewed-by:@me repo:"+strings.Join(chunk, " repo:"), me)
		if err != nil {
			return nil, err
		}
		for _, n := range nodes {
			add(n, Reason{Kind: ReasonWatched})
		}
	}

	queue := make([]PullRequest, 0, len(byID))
	for _, pr := range byID {
		queue = append(queue, *pr)
	}
	// Ties are broken by ID, so the same PRs always give the same order and the same hash.
	slices.SortFunc(queue, func(a, b PullRequest) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return queue, nil
}

// requestReason lists the requested teams, unless the request went to the user directly.
func requestReason(n prNode, me string) Reason {
	var teams []string
	for _, rr := range n.ReviewRequests.Nodes {
		switch r := rr.RequestedReviewer; r.Typename {
		case "User":
			if r.User.Login == me {
				return Reason{Kind: ReasonRequested}
			}
		case "Team":
			teams = append(teams, r.Team.CombinedSlug)
		}
	}
	return Reason{Kind: ReasonRequested, ViaTeam: true, Teams: teams}
}

func toPullRequest(n prNode) *PullRequest {
	author := n.Author.Login
	if author == "" {
		author = "ghost" // GitHub's name for deleted accounts
	}
	pr := &PullRequest{
		ID: n.ID, Repo: n.Repository.NameWithOwner, Number: n.Number, Title: n.Title, URL: n.URL,
		Author: author, IsDraft: n.IsDraft, CreatedAt: n.CreatedAt, UpdatedAt: n.UpdatedAt,
		Additions: n.Additions, Deletions: n.Deletions, ChangedFiles: n.ChangedFiles, HeadSHA: n.HeadRefOid,
	}
	if len(n.Commits.Nodes) > 0 {
		pr.CIState = n.Commits.Nodes[0].Commit.StatusCheckRollup.State
	}
	return pr
}
