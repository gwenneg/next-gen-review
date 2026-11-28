package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// DraftStore is the part of store.Store the posting handlers use.
type DraftStore interface {
	Review(ctx context.Context, id int64) (review.Review, bool, error)
	Posting(ctx context.Context, id int64) (review.Posting, error)
	SetMainComment(ctx context.Context, id int64, body string) error
	AddComment(ctx context.Context, id int64, c review.Comment) (review.Comment, error)
	SetComment(ctx context.Context, id, commentID int64, body string) error
	DeleteComment(ctx context.Context, id, commentID int64) error
	MarkPosted(ctx context.Context, id int64, url, event string) error
}

// Poster is the part of github.Poster the handlers use.
type Poster interface {
	PostReview(ctx context.Context, repo string, number int, commit, event, body string, comments []review.Comment) (string, error)
	FindReview(ctx context.Context, repo string, number int, commit, author, body string) (string, bool, error)
}

// badRequest is an error in the request, answered with 400.
type badRequest string

func (e badRequest) Error() string { return string(e) }

// postingResponse is a review's comments for GitHub, and who posting would post as.
type postingResponse struct {
	review.Posting
	PostingEnabled bool   `json:"postingEnabled"`
	PostsAs        string `json:"postsAs"`
}

// posting runs a change to the review's comments for GitHub, if any, and answers with them as
// they are now. Changes run one at a time: an edit made while a post is on its way to GitHub would
// be stored but not posted, so it waits, then finds the comments posted.
func (d Deps) posting(change func(r *http.Request, id int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err == nil && change != nil {
			d.postMu.Lock()
			err = change(r, id)
			d.postMu.Unlock()
		} else if err != nil {
			err = badRequest("bad review id")
		}
		var p review.Posting
		if err == nil {
			p, err = d.Drafts.Posting(r.Context(), id)
		}
		var bad badRequest
		switch {
		case errors.As(err, &bad):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, review.ErrUnknownReview), errors.Is(err, review.ErrUnknownComment):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, review.ErrActive), errors.Is(err, review.ErrPosted), errors.Is(err, errPostingOff):
			writeError(w, http.StatusConflict, err.Error())
		case errors.As(err, new(githubError)), errors.Is(err, errMaybePosted):
			slog.Warn("post review", "review", id, "error", err)
			writeError(w, http.StatusBadGateway, err.Error())
		case errors.Is(err, errNotRecorded):
			slog.Error("post review", "review", id, "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
		case err != nil:
			slog.Error("review comments", "review", id, "error", err)
			writeError(w, http.StatusInternalServerError, "could not change the review's comments")
		default:
			writeJSON(w, http.StatusOK, postingResponse{Posting: p, PostingEnabled: d.Poster != nil, PostsAs: d.PosterLogin})
		}
	}
}

func decode(r *http.Request, v any) error {
	// A character can take 6 bytes in JSON, as \uXXXX.
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 6*int64(review.MaxBody))).Decode(v); err != nil {
		return badRequest("send the change as JSON")
	}
	return nil
}

// checkBody refuses a comment GitHub wouldn't take once the AI label is added.
func checkBody(body string) error {
	if utf8.RuneCountInString(body) > review.MaxComment {
		return badRequest(fmt.Sprintf("a comment can be at most %d characters", review.MaxComment))
	}
	return nil
}

func (d Deps) setMain(r *http.Request, id int64) error {
	var in struct{ Body string }
	if err := decode(r, &in); err != nil {
		return err
	}
	if err := checkBody(in.Body); err != nil {
		return err
	}
	return d.Drafts.SetMainComment(r.Context(), id, in.Body)
}

func (d Deps) addComment(r *http.Request, id int64) error {
	var c review.Comment
	if err := decode(r, &c); err != nil {
		return err
	}
	switch {
	case c.Path == "" || c.Line < 1:
		return badRequest("a comment needs a file path and a line")
	case c.Side != review.SideRight && c.Side != review.SideLeft:
		return badRequest("side is RIGHT (the new code) or LEFT (deleted lines)")
	case c.StartLine < 0 || c.StartLine > c.Line:
		return badRequest("a range's first line must come before its last")
	case c.Body == "":
		return badRequest("the comment is empty")
	}
	if err := checkBody(c.Body); err != nil {
		return err
	}
	_, err := d.Drafts.AddComment(r.Context(), id, c)
	return err
}

func commentID(r *http.Request) (int64, error) {
	cid, err := strconv.ParseInt(r.PathValue("cid"), 10, 64)
	if err != nil {
		return 0, badRequest("bad comment id")
	}
	return cid, nil
}

func (d Deps) setComment(r *http.Request, id int64) error {
	cid, err := commentID(r)
	if err != nil {
		return err
	}
	var in struct{ Body string }
	if err := decode(r, &in); err != nil {
		return err
	}
	if in.Body == "" {
		return badRequest("the comment is empty: delete it instead")
	}
	if err := checkBody(in.Body); err != nil {
		return err
	}
	return d.Drafts.SetComment(r.Context(), id, cid, in.Body)
}

func (d Deps) deleteComment(r *http.Request, id int64) error {
	cid, err := commentID(r)
	if err != nil {
		return err
	}
	return d.Drafts.DeleteComment(r.Context(), id, cid)
}

var (
	errPostingOff = errors.New("posting is off: set GH_REVIEW_TOKEN, a token that can write PR reviews, and restart the api")
	// errMaybePosted means GitHub didn't answer the post: it may have the review. Posting again is
	// safe, since post looks for it on GitHub first.
	errMaybePosted = errors.New("GitHub didn't confirm the review, so it may be posted: check the PR, " +
		"or post again, which records the review if GitHub has it instead of posting it twice")
	// errNotRecorded means GitHub has the review but the database doesn't: posting again would
	// post it twice.
	errNotRecorded = errors.New("the review is on GitHub, but recording it failed: don't post it again")
)

// githubError is an error from GitHub, answered with 502 and its own message.
type githubError struct{ error }

func (e githubError) Unwrap() error { return e.error }

// post posts the comments as one GitHub review, with the maintainer's verdict, at the reviewed
// commit. Comments GitHub doesn't accept inline go in the main comment. Every comment ends with
// the AI label.
func (d Deps) post(r *http.Request, id int64) error {
	var in struct{ Event string }
	if err := decode(r, &in); err != nil {
		return err
	}
	if !slices.Contains(review.Events, in.Event) {
		return badRequest("the verdict is COMMENT, APPROVE or REQUEST_CHANGES")
	}
	if d.Poster == nil {
		return errPostingOff
	}
	if d.PosterLogin == "" { // FindReview needs it
		return fmt.Errorf("%w: GitHub didn't say who GH_REVIEW_TOKEN belongs to at startup: check the token, then restart the api", errPostingOff)
	}
	rv, ok, err := d.Drafts.Review(r.Context(), id)
	switch {
	case err != nil:
		return err
	case !ok:
		return review.ErrUnknownReview
	case rv.Active():
		return review.ErrActive
	}
	p, err := d.Drafts.Posting(r.Context(), id)
	if err != nil {
		return err
	}
	if p.Posted {
		return review.ErrPosted
	}
	var inline, offDiff []review.Comment
	for _, c := range p.Comments {
		if c.InDiff {
			c.Body = review.CommentBody(c.Body)
			if utf8.RuneCountInString(c.Body) > review.MaxBody {
				return badRequest(fmt.Sprintf("the comment on %s:%d is too long for GitHub: shorten it", c.Path, c.Line))
			}
			inline = append(inline, c)
		} else {
			offDiff = append(offDiff, c)
		}
	}
	body := review.MainBody(p.Main, offDiff)
	if n := utf8.RuneCountInString(body); n > review.MaxBody {
		return badRequest(fmt.Sprintf("the main comment, with the comments outside the diff, has %d characters, and GitHub "+
			"takes at most %d: shorten them", n, review.MaxBody))
	}
	// The queue only has public repos, but a repo can turn private.
	if _, err := d.Repos.PublicRepo(r.Context(), rv.Repo); err != nil {
		return githubError{fmt.Errorf("check that %s is public: %w", rv.Repo, err)}
	}
	// The post isn't canceled if the page leaves: GitHub may have it, and it must be recorded.
	ctx := context.WithoutCancel(r.Context())
	// An earlier post may have reached GitHub without its answer reaching here.
	url, found, err := d.Poster.FindReview(ctx, rv.Repo, rv.Number, rv.HeadSHA, d.PosterLogin, body)
	if err != nil {
		return githubError{fmt.Errorf("look for an earlier post of this review: %w", err)}
	}
	if !found {
		url, err = d.Poster.PostReview(ctx, rv.Repo, rv.Number, rv.HeadSHA, in.Event, body, inline)
		switch {
		case errors.Is(err, github.ErrRefused):
			return githubError{err}
		case err != nil:
			return fmt.Errorf("%w: %w", errMaybePosted, err)
		}
	}
	if err := d.Drafts.MarkPosted(ctx, id, url, in.Event); err != nil {
		return fmt.Errorf("%w (%s): %w", errNotRecorded, url, err)
	}
	return nil
}
