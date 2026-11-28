package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

var (
	// ErrNotInQueue means the PR isn't waiting for the user, so it can't be reviewed.
	ErrNotInQueue = errors.New("this PR is not in the queue")
	// ErrActiveReview means the PR already has a review queued or running.
	ErrActiveReview = errors.New("this PR already has a review queued or running")
)

const reviewColumns = `id, pr_id, repo, number, head_sha, model, effort, token_cap, context, status,
	cancel_requested, session_id::text, tokens_used, result, error, created_at, started_at, finished_at, posted_url`

func scanReview(row pgx.CollectableRow) (review.Review, error) {
	var r review.Review
	err := row.Scan(&r.ID, &r.PRID, &r.Repo, &r.Number, &r.HeadSHA, &r.Model, &r.Effort, &r.TokenCap, &r.Context,
		&r.Status, &r.CancelRequested, &r.SessionID, &r.TokensUsed, &r.Result, &r.Error, &r.CreatedAt, &r.StartedAt,
		&r.FinishedAt, &r.PostedURL)
	return r, err
}

// CreateReview queues a review of a PR from the queue, at the head commit the queue has for it.
func (s *Store) CreateReview(ctx context.Context, req review.Request) (review.Review, error) {
	var r review.Review
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `INSERT INTO reviews (pr_id, repo, number, head_sha, model, effort, token_cap, context)
			SELECT id, repo, number, head_sha, $2, $3, $4, $5 FROM queue_items WHERE id = $1
			RETURNING `+reviewColumns, req.PRID, req.Model, req.Effort, req.TokenCap, req.Context)
		if err != nil {
			return err
		}
		r, err = pgx.CollectExactlyOneRow(rows, scanReview)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotInQueue
		}
		if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok && pgErr.ConstraintName == "reviews_one_active_per_pr" {
			return ErrActiveReview
		}
		if err != nil {
			return err
		}
		return addEvent(ctx, tx, review.EventChanged)
	})
	return r, err
}

// Reviews returns the 100 latest reviews, or only the active ones, newest first.
func (s *Store) Reviews(ctx context.Context, activeOnly bool) ([]review.Review, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reviewColumns+` FROM reviews
		WHERE NOT $1 OR status IN ('queued', 'preparing', 'running') ORDER BY id DESC LIMIT 100`, activeOnly)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, scanReview)
}

// Review returns one review, or false if no review has this id.
func (s *Store) Review(ctx context.Context, id int64) (review.Review, bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+reviewColumns+` FROM reviews WHERE id = $1`, id)
	if err != nil {
		return review.Review{}, false, err
	}
	r, err := pgx.CollectExactlyOneRow(rows, scanReview)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// ClaimReview moves the oldest queued review to preparing and returns it, or returns false. The
// event is in the same transaction, so a failure can't leave a claimed review nobody prepares.
func (s *Store) ClaimReview(ctx context.Context) (review.Review, bool, error) {
	var r review.Review
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `UPDATE reviews SET status = 'preparing', started_at = now()
			WHERE id = (SELECT id FROM reviews WHERE status = 'queued' ORDER BY id LIMIT 1 FOR UPDATE SKIP LOCKED)
			RETURNING `+reviewColumns)
		if err != nil {
			return err
		}
		if r, err = pgx.CollectExactlyOneRow(rows, scanReview); err != nil {
			return err
		}
		return addEvent(ctx, tx, review.EventChanged)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// StartReview stores the prompt and the diff's lines, and moves the review to running. It returns
// false if the user canceled it while it was being prepared.
func (s *Store) StartReview(ctx context.Context, id int64, prompt string, lines review.DiffLines) (bool, error) {
	if lines == nil {
		lines = review.DiffLines{}
	}
	return s.update(ctx, `UPDATE reviews SET status = 'running', prompt = $2, diff_lines = $3
		WHERE id = $1 AND status = 'preparing' AND NOT cancel_requested`, id, prompt, lines)
}

// CancelRequested says whether the user asked to cancel the review.
func (s *Store) CancelRequested(ctx context.Context, id int64) (bool, error) {
	var c bool
	err := s.pool.QueryRow(ctx, `SELECT cancel_requested FROM reviews WHERE id = $1`, id).Scan(&c)
	return c, err
}

// ReviewProgress stores a running review's token count and returns whether the worker must stop:
// the user asked to cancel, or the review is no longer running (for example after the broker failed
// it because the worker had stopped reporting), so its tokens would be spent for nothing.
func (s *Store) ReviewProgress(ctx context.Context, id int64, tokens int) (stop bool, err error) {
	err = s.pool.QueryRow(ctx, `UPDATE reviews SET tokens_used = $2 WHERE id = $1
		RETURNING cancel_requested OR status <> 'running'`, id, tokens).Scan(&stop)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return stop, s.notify(ctx, review.EventProgress)
}

// CancelReview cancels a queued review at once, or asks the worker to stop a preparing or running
// one. It returns false if the review isn't active.
func (s *Store) CancelReview(ctx context.Context, id int64) (bool, error) {
	return s.update(ctx, `UPDATE reviews SET
		cancel_requested = true,
		status = CASE WHEN status = 'queued' THEN 'canceled' ELSE status END,
		finished_at = CASE WHEN status = 'queued' THEN now() ELSE finished_at END
		WHERE id = $1 AND status IN ('queued', 'preparing', 'running')`, id)
}

// FinishReview stores how a review ended, with the comments the AI drafted. The texts come from
// claude and from file names in the PR, so NUL bytes and invalid UTF-8, which Postgres refuses in
// text, are dropped or replaced.
func (s *Store) FinishReview(ctx context.Context, id int64, o review.Outcome) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE reviews SET status = $2, result = $3, error = $4, tokens_used = $5,
			finished_at = now() WHERE id = $1 AND status IN ('preparing', 'running')`, id, o.Status, text(o.Result), text(o.Error), o.Tokens)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("review %d: %w", id, review.ErrNotActive)
		}
		if o.Drafts != nil {
			if _, err := tx.Exec(ctx, `UPDATE reviews SET main_comment = $2 WHERE id = $1`, id, text(o.Drafts.Main)); err != nil {
				return err
			}
			for _, c := range o.Drafts.Comments {
				if err := insertComment(ctx, tx, id, &c); err != nil {
					return err
				}
			}
		}
		return addEvent(ctx, tx, review.EventChanged)
	})
	return err
}

// update runs an UPDATE of a review and, if it changed one, adds the event in the same
// transaction. So a failed event can't leave a change saved that the caller thinks failed.
func (s *Store) update(ctx context.Context, sql string, args ...any) (changed bool, err error) {
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, sql, args...)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		changed = true
		return addEvent(ctx, tx, review.EventChanged)
	})
	return changed && err == nil, err
}

// text makes s storable in a Postgres text column.
func text(s string) string {
	return strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "\uFFFD")
}

// FailInterruptedReviews fails the reviews the api was preparing when it stopped. The running ones
// are the worker's: the broker adopts them (see RunningReviews).
func (s *Store) FailInterruptedReviews(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `UPDATE reviews SET status = 'failed', error = 'interrupted: the api restarted',
		finished_at = now() WHERE status = 'preparing'`)
	return err
}

// RunningReviews returns the ids of the running reviews.
func (s *Store) RunningReviews(ctx context.Context) ([]int64, error) {
	rows, err := s.pool.Query(ctx, `SELECT id FROM reviews WHERE status = 'running' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[int64])
}

// notify tells the pages about a short-lived change without storing an event.
func (s *Store) notify(ctx context.Context, eventType string) error {
	_, err := s.pool.Exec(ctx, `SELECT pg_notify('events', json_build_object('seq', 0, 'type', $1::text)::text)`, eventType)
	return err
}
