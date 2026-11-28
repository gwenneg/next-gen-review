// Command api serves the HTTP API and WebSocket, and runs the database migrations at startup.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cli/go-gh/v2/pkg/auth"

	"github.com/gwenneg/next-gen-review/backend/internal/db"
	"github.com/gwenneg/next-gen-review/backend/internal/events"
	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/queue"
	"github.com/gwenneg/next-gen-review/backend/internal/review"
	"github.com/gwenneg/next-gen-review/backend/internal/server"
	"github.com/gwenneg/next-gen-review/backend/internal/store"
)

const (
	addr         = ":8080"
	internalAddr = ":8081" // the worker's API, reachable only on the Compose network
	pollInterval = 2 * time.Minute
	checkouts    = "/checkouts" // shared with the worker, which mounts it read-only
)

func main() {
	// The runtime image has no curl, so the container health check calls the binary itself.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("api stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := db.Open(ctx)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := db.Migrate(ctx, pool); err != nil {
		return err
	}

	st := store.New(pool)
	// GH_TOKEN first, then GITHUB_TOKEN, like the gh CLI.
	token, source := auth.TokenFromEnvOrConfig("github.com")
	slog.Info("GitHub token", "source", source)
	gh, err := github.NewClient(token)
	if err != nil {
		return err
	}
	poller := queue.NewPoller(gh, st, pollInterval)
	hub := events.NewHub()
	go hub.Listen(ctx)
	go poller.Run(ctx)

	if err := st.FailInterruptedReviews(ctx); err != nil {
		return err
	}
	broker := review.NewBroker(st, gh, checkouts)
	if err := broker.RemoveCheckouts(); err != nil {
		return err
	}
	go broker.Run(ctx)

	handler := server.New(server.Deps{DB: pool, Store: st, Poller: poller, Repos: gh, Events: hub, TokenSource: source,
		Reviews: st, Broker: broker})
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	internal := &http.Server{Addr: internalAddr, Handler: server.NewInternal(broker), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 2)
	go func() { errc <- srv.ListenAndServe() }()
	go func() { errc <- internal.ListenAndServe() }()
	slog.Info("api listening", "addr", addr, "internal", internalAddr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = internal.Shutdown(shutdownCtx)
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func healthcheck() int {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + addr + "/api/health")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "health:", resp.Status)
		return 1
	}
	return 0
}
