// Package store reads and writes the api's tables.
package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gwenneg/next-gen-review/backend/internal/github"
)

// EventQueueChanged tells the browsers to reload the queue.
const EventQueueChanged = "queue.changed"

// Store wraps the database pool.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// WatchedRepos returns the watched repos, sorted by name.
func (s *Store) WatchedRepos(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT full_name FROM watched_repos ORDER BY lower(full_name)`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// AddWatchedRepo adds a repo. Adding a repo that's already watched does nothing.
func (s *Store) AddWatchedRepo(ctx context.Context, fullName string) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO watched_repos (full_name) VALUES ($1) ON CONFLICT DO NOTHING`, fullName)
	return err
}

// RemoveWatchedRepo removes a repo, ignoring case. It returns false if the repo wasn't watched.
func (s *Store) RemoveWatchedRepo(ctx context.Context, fullName string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM watched_repos WHERE lower(full_name) = lower($1)`, fullName)
	return tag.RowsAffected() > 0, err
}

// Queue returns the queue as of the last poll, oldest first.
func (s *Store) Queue(ctx context.Context) ([]github.PullRequest, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, repo, number, title, url, author, is_draft, created_at, updated_at,
		additions, deletions, changed_files, head_sha, ci_state, reasons FROM queue_items ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (github.PullRequest, error) {
		var pr github.PullRequest
		var reasons []byte
		err := row.Scan(&pr.ID, &pr.Repo, &pr.Number, &pr.Title, &pr.URL, &pr.Author, &pr.IsDraft, &pr.CreatedAt,
			&pr.UpdatedAt, &pr.Additions, &pr.Deletions, &pr.ChangedFiles, &pr.HeadSHA, &pr.CIState, &reasons)
		if err != nil {
			return pr, err
		}
		return pr, json.Unmarshal(reasons, &pr.Reasons)
	})
}

// ReplaceQueue replaces the queue and records a queue.changed event, in one transaction.
func (s *Store) ReplaceQueue(ctx context.Context, queue []github.PullRequest) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM queue_items`); err != nil {
			return err
		}
		batch := &pgx.Batch{}
		for _, pr := range queue {
			reasons, err := json.Marshal(pr.Reasons)
			if err != nil {
				return err
			}
			batch.Queue(`INSERT INTO queue_items (id, repo, number, title, url, author, is_draft, created_at,
				updated_at, additions, deletions, changed_files, head_sha, ci_state, reasons)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
				pr.ID, pr.Repo, pr.Number, pr.Title, pr.URL, pr.Author, pr.IsDraft, pr.CreatedAt, pr.UpdatedAt,
				pr.Additions, pr.Deletions, pr.ChangedFiles, pr.HeadSHA, pr.CIState, reasons)
		}
		if err := tx.SendBatch(ctx, batch).Close(); err != nil {
			return fmt.Errorf("insert queue: %w", err)
		}
		return addEvent(ctx, tx, EventQueueChanged)
	})
}

// AddEvent records an event and notifies the listeners.
func (s *Store) AddEvent(ctx context.Context, eventType string) error {
	return addEvent(ctx, s.pool, eventType)
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// addEvent inserts the event and sends its seq and type with NOTIFY. Postgres delivers the
// notification when the transaction commits, so listeners never see an uncommitted event.
func addEvent(ctx context.Context, db execer, eventType string) error {
	_, err := db.Exec(ctx, `WITH e AS (INSERT INTO events (type) VALUES ($1) RETURNING seq, type)
		SELECT pg_notify('events', json_build_object('seq', seq, 'type', type)::text) FROM e`, eventType)
	return err
}
