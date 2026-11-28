package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
)

// Posting returns a review's comments for GitHub. Each comment says whether GitHub accepts it
// inline, from the reviewed commit's diff.
func (s *Store) Posting(ctx context.Context, id int64) (review.Posting, error) {
	var p review.Posting
	err := s.pool.QueryRow(ctx, `SELECT main_comment, posted_at IS NOT NULL, posted_url, posted_event, diff_lines
		FROM reviews WHERE id = $1`, id).Scan(&p.Main, &p.Posted, &p.PostedURL, &p.PostedEvent, &p.Lines)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, review.ErrUnknownReview
	}
	if err != nil {
		return p, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, path, line, coalesce(start_line, 0), side, body FROM review_comments
		WHERE review_id = $1 ORDER BY path, line, id`, id)
	if err != nil {
		return p, err
	}
	p.Comments, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (review.Comment, error) {
		var c review.Comment
		err := row.Scan(&c.ID, &c.Path, &c.Line, &c.StartLine, &c.Side, &c.Body)
		c.InDiff = p.Lines.Accepts(c)
		return c, err
	})
	return p, err
}

// editDrafts runs edit in a transaction, once the review has ended and while its comments aren't
// posted. It locks the review, so edits and posting don't overlap.
func (s *Store) editDrafts(ctx context.Context, id int64, edit func(tx pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var status string
		var posted *time.Time
		err := tx.QueryRow(ctx, `SELECT status, posted_at FROM reviews WHERE id = $1 FOR UPDATE`, id).Scan(&status, &posted)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return review.ErrUnknownReview
		case err != nil:
			return err
		case (review.Review{Status: status}).Active():
			return review.ErrActive
		case posted != nil:
			return review.ErrPosted
		}
		return edit(tx)
	})
}

// SetMainComment changes the review's main comment.
func (s *Store) SetMainComment(ctx context.Context, id int64, body string) error {
	return s.editDrafts(ctx, id, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE reviews SET main_comment = $2 WHERE id = $1`, id, text(body))
		return err
	})
}

// AddComment adds an inline comment and returns it with its id.
func (s *Store) AddComment(ctx context.Context, id int64, c review.Comment) (review.Comment, error) {
	err := s.editDrafts(ctx, id, func(tx pgx.Tx) error { return insertComment(ctx, tx, id, &c) })
	return c, err
}

func insertComment(ctx context.Context, tx pgx.Tx, id int64, c *review.Comment) error {
	var start *int
	if c.StartLine != 0 {
		start = &c.StartLine
	}
	return tx.QueryRow(ctx, `INSERT INTO review_comments (review_id, path, line, start_line, side, body)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, id, text(c.Path), c.Line, start, c.Side, text(c.Body)).Scan(&c.ID)
}

// SetComment changes an inline comment's text.
func (s *Store) SetComment(ctx context.Context, id, commentID int64, body string) error {
	return s.editDrafts(ctx, id, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE review_comments SET body = $3 WHERE review_id = $1 AND id = $2`, id, commentID, text(body))
		if err == nil && tag.RowsAffected() == 0 {
			return review.ErrUnknownComment
		}
		return err
	})
}

// DeleteComment deletes an inline comment.
func (s *Store) DeleteComment(ctx context.Context, id, commentID int64) error {
	return s.editDrafts(ctx, id, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM review_comments WHERE review_id = $1 AND id = $2`, id, commentID)
		if err == nil && tag.RowsAffected() == 0 {
			return review.ErrUnknownComment
		}
		return err
	})
}

// MarkPosted records the GitHub review the comments were posted as. From then on they can't change.
func (s *Store) MarkPosted(ctx context.Context, id int64, url, event string) error {
	return s.editDrafts(ctx, id, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE reviews SET posted_url = $2, posted_event = $3, posted_at = now() WHERE id = $1`,
			id, url, event); err != nil {
			return fmt.Errorf("record the posted review: %w", err)
		}
		return addEvent(ctx, tx, review.EventChanged)
	})
}
