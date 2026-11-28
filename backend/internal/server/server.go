// Package server serves the HTTP API and the WebSocket used for live updates.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/gwenneg/next-gen-review/backend/internal/events"
	"github.com/gwenneg/next-gen-review/backend/internal/github"
	"github.com/gwenneg/next-gen-review/backend/internal/queue"
)

// Pinger is the part of the database pool the health check needs.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Store is the part of store.Store the handlers use.
type Store interface {
	Queue(ctx context.Context) ([]github.PullRequest, error)
	WatchedRepos(ctx context.Context) ([]string, error)
	AddWatchedRepo(ctx context.Context, fullName string) error
	RemoveWatchedRepo(ctx context.Context, fullName string) (bool, error)
}

// Poller is the part of queue.Poller the handlers use.
type Poller interface {
	Status() queue.Status
	Poll(ctx context.Context) error
	Wake()
}

// RepoChecker checks that a repo can be watched.
type RepoChecker interface {
	PublicRepo(ctx context.Context, fullName string) (string, error)
}

// Subscriber gives a WebSocket client its events.
type Subscriber interface {
	Subscribe() (<-chan events.Event, func())
}

// Deps are what the handlers need.
type Deps struct {
	DB      Pinger
	Store   Store
	Poller  Poller
	Repos   RepoChecker
	Events  Subscriber
	Reviews ReviewStore
	Broker  Broker
	// TokenSource names the variable the GitHub token came from, shown on the page.
	TokenSource string
	Drafts      DraftStore
	// Poster posts reviews to GitHub with GH_REVIEW_TOKEN, as PosterLogin. Nil when posting is off.
	Poster      Poster
	PosterLogin string

	postMu *sync.Mutex // one post at a time, so a double click can't post twice
}

// pingInterval keeps idle WebSockets open: nginx closes a connection after 1h without traffic.
const pingInterval = 30 * time.Second

// New returns the API handler.
func New(d Deps) http.Handler {
	d.postMu = &sync.Mutex{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", d.health)
	mux.HandleFunc("GET /api/ws", d.events)
	mux.HandleFunc("GET /api/queue", d.queue)
	mux.HandleFunc("POST /api/queue/refresh", d.refresh)
	mux.HandleFunc("GET /api/watched-repos", d.watchedRepos)
	mux.HandleFunc("POST /api/watched-repos", d.addWatchedRepo)
	mux.HandleFunc("DELETE /api/watched-repos/{owner}/{name}", d.removeWatchedRepo)
	mux.HandleFunc("GET /api/review-options", d.reviewOptions)
	mux.HandleFunc("GET /api/reviews", d.reviews)
	mux.HandleFunc("POST /api/reviews", d.startReview)
	mux.HandleFunc("POST /api/reviews/{id}/cancel", d.cancelReview)
	mux.HandleFunc("GET /api/reviews/{id}/posting", d.posting(nil))
	mux.HandleFunc("PUT /api/reviews/{id}/posting/main", d.posting(d.setMain))
	mux.HandleFunc("POST /api/reviews/{id}/posting/comments", d.posting(d.addComment))
	mux.HandleFunc("PUT /api/reviews/{id}/posting/comments/{cid}", d.posting(d.setComment))
	mux.HandleFunc("DELETE /api/reviews/{id}/posting/comments/{cid}", d.posting(d.deleteComment))
	mux.HandleFunc("POST /api/reviews/{id}/posting/post", d.posting(d.post))
	// CrossOriginProtection rejects POST and DELETE requests sent by pages on other sites (CSRF).
	return localOnly(http.NewCrossOriginProtection().Handler(mux))
}

// localOnly rejects requests whose Host isn't a loopback name. With DNS rebinding, a page on
// another site makes its own hostname resolve to 127.0.0.1, so its requests reach the api with
// that hostname in Host. Browsers set Host themselves, so a page can't fake a loopback one.
func localOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		switch host {
		case "localhost", "127.0.0.1", "::1", "[::1]":
			next.ServeHTTP(w, r)
		default:
			http.Error(w, "host not allowed", http.StatusForbidden)
		}
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (d Deps) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := d.DB.Ping(ctx); err != nil {
		slog.Warn("health check: database unreachable", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "database": "unreachable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "database": "ok"})
}

type queueResponse struct {
	Items       []github.PullRequest `json:"items"`
	Status      queue.Status         `json:"status"`
	TokenSource string               `json:"tokenSource"`
}

func (d Deps) queue(w http.ResponseWriter, r *http.Request) {
	items, err := d.Store.Queue(r.Context())
	if err != nil {
		slog.Error("read queue", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the queue")
		return
	}
	writeJSON(w, http.StatusOK, queueResponse{Items: items, Status: d.Poller.Status(), TokenSource: d.TokenSource})
}

// refresh polls GitHub now and returns the new queue. A failed poll shows in the status. The poll
// isn't cancelled if the client leaves, which would store "context canceled" as GitHub's error.
func (d Deps) refresh(w http.ResponseWriter, r *http.Request) {
	_ = d.Poller.Poll(context.WithoutCancel(r.Context()))
	d.queue(w, r)
}

func (d Deps) watchedRepos(w http.ResponseWriter, r *http.Request) {
	repos, err := d.Store.WatchedRepos(r.Context())
	if err != nil {
		slog.Error("read watched repos", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the watched repos")
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"repos": repos})
}

func (d Deps) addWatchedRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Repo string `json:"repo"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, `send JSON like {"repo": "owner/name"}`)
		return
	}
	name, err := d.Repos.PublicRepo(r.Context(), body.Repo)
	if _, invalid := errors.AsType[github.InvalidRepoError](err); invalid {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		slog.Warn("check repo on GitHub", "repo", body.Repo, "error", err)
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := d.Store.AddWatchedRepo(r.Context(), name); err != nil {
		slog.Error("add watched repo", "error", err)
		writeError(w, http.StatusInternalServerError, "could not save the repo")
		return
	}
	d.Poller.Wake()
	writeJSON(w, http.StatusCreated, map[string]string{"repo": name})
}

func (d Deps) removeWatchedRepo(w http.ResponseWriter, r *http.Request) {
	removed, err := d.Store.RemoveWatchedRepo(r.Context(), r.PathValue("owner")+"/"+r.PathValue("name"))
	if err != nil {
		slog.Error("remove watched repo", "error", err)
		writeError(w, http.StatusInternalServerError, "could not remove the repo")
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "not a watched repo")
		return
	}
	d.Poller.Wake()
	w.WriteHeader(http.StatusNoContent)
}

// events upgrades to a WebSocket, sends a hello message, then forwards every event.
func (d Deps) events(w http.ResponseWriter, r *http.Request) {
	// Accept rejects browser requests whose Origin doesn't match the Host header, so another
	// website open in the browser can't connect. localOnly covers DNS rebinding, where both
	// match. Don't set InsecureSkipVerify.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	sub, unsubscribe := d.Events.Subscribe()
	defer unsubscribe()
	ctx := conn.CloseRead(r.Context())
	if err := wsjson.Write(ctx, conn, map[string]string{"type": "hello"}); err != nil {
		return
	}
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case e, ok := <-sub:
			if !ok { // fell behind: the client reconnects and reloads
				conn.Close(websocket.StatusTryAgainLater, "too many events")
				return
			}
			if err := wsjson.Write(ctx, conn, e); err != nil {
				return
			}
		case <-ping.C:
			if err := conn.Ping(ctx); err != nil {
				return
			}
		}
	}
}
