package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/review"
	"github.com/gwenneg/next-gen-review/backend/internal/store"
)

// ReviewStore is the part of store.Store the review handlers use.
type ReviewStore interface {
	CreateReview(ctx context.Context, req review.Request) (review.Review, error)
	Reviews(ctx context.Context, activeOnly bool) ([]review.Review, error)
	CancelReview(ctx context.Context, id int64) (bool, error)
}

// Broker is the part of review.Broker the handlers use.
type Broker interface {
	Wake()
	Next(ctx context.Context, wait time.Duration) (review.Job, bool, error)
	Progress(ctx context.Context, id int64, tokens int) (bool, error)
	Finish(ctx context.Context, id int64, o review.Outcome)
	WorkerConnected() bool
}

func (d Deps) reviewOptions(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, review.AvailableOptions)
}

type reviewsResponse struct {
	Reviews []review.Review `json:"reviews"`
	// WorkerConnected is false when the worker hasn't called the api lately, so queued reviews
	// won't start: it is stopped, or can't reach the api.
	WorkerConnected bool `json:"workerConnected"`
}

// reviews lists the latest reviews. With ?active=true it lists only the active ones, which the
// page reloads on each progress report: they have no result yet, so the response stays small.
func (d Deps) reviews(w http.ResponseWriter, r *http.Request) {
	list, err := d.Reviews.Reviews(r.Context(), r.URL.Query().Get("active") == "true")
	if err != nil {
		slog.Error("read reviews", "error", err)
		writeError(w, http.StatusInternalServerError, "could not read the reviews")
		return
	}
	writeJSON(w, http.StatusOK, reviewsResponse{Reviews: list, WorkerConnected: d.Broker.WorkerConnected()})
}

func (d Deps) startReview(w http.ResponseWriter, r *http.Request) {
	var req review.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "send the review options as JSON")
		return
	}
	if err := req.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rv, err := d.Reviews.CreateReview(r.Context(), req)
	switch {
	case errors.Is(err, store.ErrNotInQueue):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrActiveReview):
		writeError(w, http.StatusConflict, err.Error())
	case err != nil:
		slog.Error("create review", "error", err)
		writeError(w, http.StatusInternalServerError, "could not start the review")
	default:
		d.Broker.Wake()
		writeJSON(w, http.StatusCreated, rv)
	}
}

func (d Deps) cancelReview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad review id")
		return
	}
	ok, err := d.Reviews.CancelReview(r.Context(), id)
	switch {
	case err != nil:
		slog.Error("cancel review", "error", err)
		writeError(w, http.StatusInternalServerError, "could not cancel the review")
	case !ok:
		writeError(w, http.StatusNotFound, "no active review with this id")
	default:
		d.Broker.Wake() // a prepared review may be waiting for a busy or missing worker
		w.WriteHeader(http.StatusNoContent)
	}
}

// NewInternal returns the worker's API. It listens on a port that only the Compose network
// reaches: nginx doesn't forward to it and it isn't published.
func NewInternal(b Broker) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/jobs/next", func(w http.ResponseWriter, r *http.Request) {
		job, ok, err := b.Next(r.Context(), 25*time.Second)
		switch {
		case err != nil && r.Context().Err() == nil:
			slog.Error("next job", "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
		case !ok:
			w.WriteHeader(http.StatusNoContent)
		default:
			writeJSON(w, http.StatusOK, job)
		}
	})
	mux.HandleFunc("POST /internal/jobs/{id}/progress", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var body struct {
			Tokens int `json:"tokens"`
		}
		if err == nil {
			err = json.NewDecoder(r.Body).Decode(&body)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		stop, err := b.Progress(r.Context(), id, body.Tokens)
		if err != nil {
			slog.Error("review progress", "review", id, "error", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"stop": stop})
	})
	mux.HandleFunc("POST /internal/jobs/{id}/finish", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		var o review.Outcome
		if err == nil {
			err = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&o)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		b.Finish(r.Context(), id, o)
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}
