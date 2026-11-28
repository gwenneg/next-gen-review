package review

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gwenneg/next-gen-review/backend/internal/checkout"
)

const (
	// maxCheckout limits the size of a checked-out repository.
	maxCheckout = 1 << 30
	// prepareTimeout limits downloading and unpacking a repository.
	prepareTimeout = 10 * time.Minute
	// workerTimeout is how long the worker can go without calling the api before it counts as
	// missing. An idle worker calls at least every 25s (the long poll) plus 5s after an error.
	workerTimeout = 40 * time.Second
	// jobTimeout is how long a running review can go without a progress report before it counts
	// as lost. The worker reports every 2s while a review runs.
	jobTimeout = 2 * time.Minute
)

// Store is the part of store.Store the broker uses.
type Store interface {
	ClaimReview(ctx context.Context) (Review, bool, error)
	CancelRequested(ctx context.Context, id int64) (bool, error)
	StartReview(ctx context.Context, id int64, prompt string) (bool, error)
	ReviewProgress(ctx context.Context, id int64, tokens int) (stop bool, err error)
	FinishReview(ctx context.Context, id int64, o Outcome) error
}

// GitHub is the part of github.Client the broker uses.
type GitHub interface {
	PullRequest(ctx context.Context, repo string, number int, sha string) (PR, error)
	Tarball(ctx context.Context, repo, sha string) (io.ReadCloser, error)
}

// Broker hands reviews to the worker. It runs in the api, so the worker needs neither the
// database nor GitHub: the api downloads the code into a volume the worker mounts read-only.
//
// Run prepares one review at a time, on its own context, and waits for the worker to take it in
// Next. So a slow download doesn't depend on the worker's request, and a lost request doesn't
// lose the review.
type Broker struct {
	store      Store
	gh         GitHub
	checkouts  string // the shared volume
	wake       chan struct{}
	ready      chan Job      // a prepared review, waiting for the worker
	stopped    chan struct{} // closed when Run returns
	jobTimeout time.Duration

	mu       sync.Mutex
	calls    int                 // the worker's calls in progress, such as a long poll
	lastCall time.Time           // when the worker's latest call ended
	jobs     map[int64]time.Time // running reviews, with the worker's latest report on each
}

// NewBroker returns a broker that writes checkouts under dir.
func NewBroker(st Store, gh GitHub, dir string) *Broker {
	// lastCall starts now: after an api restart, the worker retries within 5s.
	return &Broker{store: st, gh: gh, checkouts: dir, wake: make(chan struct{}, 1), ready: make(chan Job),
		stopped: make(chan struct{}), jobTimeout: jobTimeout, lastCall: time.Now(), jobs: map[int64]time.Time{}}
}

// RemoveCheckouts deletes every checkout. Call it at startup, after the interrupted reviews were
// marked failed: no review is active then, and an interrupted one never deleted its checkout.
func (b *Broker) RemoveCheckouts() error {
	entries, err := os.ReadDir(b.checkouts)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(b.checkouts, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// WorkerConnected says whether the worker is in touch: it has a call in progress or called
// recently. While it runs a review, it reports progress every few seconds.
func (b *Broker) WorkerConnected() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls > 0 || time.Since(b.lastCall) < workerTimeout
}

// workerCall records a call from the worker. Call the returned function when it ends.
func (b *Broker) workerCall() func() {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	return func() {
		b.mu.Lock()
		b.calls--
		b.lastCall = time.Now()
		b.mu.Unlock()
	}
}

// Wake tells Run that a review was queued or canceled.
func (b *Broker) Wake() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Run prepares the queued reviews one at a time and hands them to Next, until ctx is done. It also
// fails the running reviews the worker stopped reporting on.
func (b *Broker) Run(ctx context.Context) {
	defer close(b.stopped)
	go b.failLostJobs(ctx)
	for ctx.Err() == nil {
		r, ok, err := b.store.ClaimReview(ctx)
		if err != nil {
			slog.Warn("claim a review", "error", err)
		}
		if !ok {
			b.sleep(ctx)
			continue
		}
		job, err := b.prepare(ctx, r)
		if err != nil {
			slog.Warn("prepare review", "review", r.ID, "error", err)
			o := Outcome{Status: StatusFailed, Error: "prepare the review: " + err.Error()}
			if b.canceled(ctx, r.ID) {
				o = Outcome{Status: StatusCanceled}
			}
			b.Finish(ctx, r.ID, o)
			continue
		}
		b.handOver(ctx, job)
	}
}

// sleep waits for a wake, or 5s in case one was missed.
func (b *Broker) sleep(ctx context.Context) {
	select {
	case <-ctx.Done():
	case <-b.wake:
	case <-time.After(5 * time.Second):
	}
}

// handOver waits for the worker to take the job in Next. The worker may be busy with another
// review or missing, so on a wake (the cancel handler sends one), and every 5s, it checks whether
// the user canceled the job. Run claims again right after, so a wake for a new review isn't lost.
func (b *Broker) handOver(ctx context.Context, job Job) {
	for {
		select {
		case b.ready <- job:
			return
		case <-ctx.Done():
			return
		case <-b.wake:
		case <-time.After(5 * time.Second):
		}
		if b.canceled(ctx, job.ID) {
			b.Finish(ctx, job.ID, Outcome{Status: StatusCanceled})
			return
		}
	}
}

// canceled says whether the user asked to cancel the review. On an error it says no: the worker
// checks again before it runs the review.
func (b *Broker) canceled(ctx context.Context, id int64) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	c, err := b.store.CancelRequested(ctx, id)
	if err != nil {
		slog.Warn("check for a cancel", "review", id, "error", err)
	}
	return c
}

func (b *Broker) prepare(ctx context.Context, r Review) (Job, error) {
	ctx, cancel := context.WithTimeout(ctx, prepareTimeout)
	defer cancel()
	pr, err := b.gh.PullRequest(ctx, r.Repo, r.Number, r.HeadSHA) // the commit the user saw in the queue
	if err != nil {
		return Job{}, err
	}
	tarball, err := b.gh.Tarball(ctx, r.Repo, r.HeadSHA)
	if err != nil {
		return Job{}, err
	}
	defer tarball.Close()
	dir := filepath.Join(b.checkouts, strconv.FormatInt(r.ID, 10))
	if err := checkout.Extract(tarball, dir, maxCheckout); err != nil {
		return Job{}, err
	}
	return Job{ID: r.ID, Model: r.Model, Effort: r.Effort, TokenCap: r.TokenCap, SessionID: r.SessionID,
		Dir: dir, Prompt: Prompt(pr, r.Context)}, nil
}

// Next waits up to wait for a prepared review, marks it running and returns its job. It returns
// false when none is ready, or at once when Run stopped, so the api's shutdown doesn't wait for
// the long poll. A review the user canceled while it was prepared is skipped.
func (b *Broker) Next(ctx context.Context, wait time.Duration) (Job, bool, error) {
	defer b.workerCall()()
	deadline := time.After(wait)
	for {
		var job Job
		select {
		case <-ctx.Done():
			return Job{}, false, ctx.Err()
		case <-deadline:
			return Job{}, false, nil
		case <-b.stopped:
			return Job{}, false, nil
		case job = <-b.ready:
		}
		// The review is ours now: don't let the worker's request cancel the update.
		started, err := b.store.StartReview(context.WithoutCancel(ctx), job.ID, job.Prompt)
		if err != nil || !started {
			o := Outcome{Status: StatusCanceled}
			if err != nil {
				o = Outcome{Status: StatusFailed, Error: "start the review: " + err.Error()}
			}
			b.Finish(ctx, job.ID, o)
			continue
		}
		b.mu.Lock()
		b.jobs[job.ID] = time.Now()
		b.mu.Unlock()
		return job, true, nil
	}
}

// Progress stores the token count and says whether the worker must stop.
func (b *Broker) Progress(ctx context.Context, id int64, tokens int) (bool, error) {
	defer b.workerCall()()
	b.mu.Lock()
	if _, ok := b.jobs[id]; ok {
		b.jobs[id] = time.Now()
	}
	b.mu.Unlock()
	return b.store.ReviewProgress(ctx, id, tokens)
}

// failLostJobs fails the running reviews the worker stopped reporting on: it crashed, or never
// got the job because its request had already ended. Nobody else would finish them.
func (b *Broker) failLostJobs(ctx context.Context) {
	t := time.NewTicker(b.jobTimeout / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		var lost []int64
		b.mu.Lock()
		for id, seen := range b.jobs {
			if time.Since(seen) > b.jobTimeout {
				lost = append(lost, id)
			}
		}
		b.mu.Unlock()
		for _, id := range lost {
			slog.Warn("the worker stopped reporting on a review", "review", id)
			b.Finish(ctx, id, Outcome{Status: StatusFailed, Error: "the worker stopped responding"})
		}
	}
}

// Finish stores the outcome and deletes the checkout. The claude transcript stays in the worker's
// volume. It doesn't use ctx's cancellation: a review whose outcome isn't stored would stay active
// and block new reviews of its PR.
func (b *Broker) Finish(ctx context.Context, id int64, o Outcome) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	b.mu.Lock()
	delete(b.jobs, id)
	b.mu.Unlock()
	if err := b.store.FinishReview(ctx, id, o); err != nil {
		slog.Warn("finish review", "review", id, "error", err)
	}
	if err := os.RemoveAll(filepath.Join(b.checkouts, strconv.FormatInt(id, 10))); err != nil {
		slog.Warn("delete checkout", "review", id, "error", err)
	}
}
