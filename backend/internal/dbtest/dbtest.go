// Package dbtest runs a throwaway Postgres in Docker for tests, with the image compose.yaml uses
// and the app's migrations applied.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/log"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/gwenneg/next-gen-review/backend/internal/db"
)

// Main starts Postgres, sets *pool and the PG* variables (for code that connects on its own, such
// as the event listener), runs the tests and stops Postgres. Call it from TestMain.
func Main(m *testing.M, pool **pgxpool.Pool) int {
	ctx := context.Background()
	log.SetDefault(log.NewNoopLogger()) // errors still come back from Run
	image, err := composeImage()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	ctr, err := postgres.Run(ctx, image, postgres.WithDatabase("review"), postgres.WithUsername("review"),
		postgres.WithPassword("test"), postgres.BasicWaitStrategies())
	defer func() { _ = testcontainers.TerminateContainer(ctr) }()
	if err != nil {
		fmt.Fprintln(os.Stderr, "start Postgres in Docker:", err)
		return 1
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	for k, v := range map[string]string{"PGHOST": host, "PGPORT": port.Port(), "PGUSER": "review",
		"PGPASSWORD": "test", "PGDATABASE": "review", "PGSSLMODE": "disable"} {
		_ = os.Setenv(k, v)
	}
	if *pool, err = db.Open(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer (*pool).Close()
	if err := db.Migrate(ctx, *pool); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// Reset empties every table, so each test starts from a clean database.
func Reset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `TRUNCATE events, watched_repos, queue_items, reviews, review_comments RESTART IDENTITY`); err != nil {
		t.Fatal(err)
	}
}

// composeImage returns the Postgres image of compose.yaml, so the tests run the stack's version.
func composeImage() (string, error) {
	_, file, _, _ := runtime.Caller(0)
	compose, err := os.ReadFile(filepath.Join(filepath.Dir(file), "..", "..", "..", "compose.yaml"))
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`image:\s*(postgres:\S+)`).FindSubmatch(compose)
	if m == nil {
		return "", fmt.Errorf("no postgres image in compose.yaml")
	}
	return string(m[1]), nil
}
