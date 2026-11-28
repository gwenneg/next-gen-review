import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  getQueue,
  getReviewOptions,
  getReviews,
  refreshQueue,
  type QueueResponse,
  type Review,
  type ReviewOptions,
  type ReviewsResponse,
} from './api'
import { useLiveUpdates, type LiveState } from './live'
import { Queue } from './Queue'
import { WatchedRepos } from './WatchedRepos'

// Polls the API health endpoint, which also reports whether the database is reachable.
function useHealth(): LiveState {
  const [state, setState] = useState<LiveState>('connecting')
  useEffect(() => {
    const check = () =>
      fetch('/api/health')
        .then((r) => setState(r.ok ? 'ok' : 'bad'))
        .catch(() => setState('bad'))
    check()
    const id = setInterval(check, 10_000)
    return () => clearInterval(id)
  }, [])
  return state
}

function Status({ label, state }: { label: string; state: LiveState }) {
  return (
    <span title={state}>
      <span className={`dot ${state === 'connecting' ? '' : state}`} />
      {label}
    </span>
  )
}

export default function App() {
  const [queue, setQueue] = useState<QueueResponse>()
  const [loadError, setLoadError] = useState<string>()
  const [refreshing, setRefreshing] = useState(false)

  const load = useCallback(() => {
    getQueue()
      .then((q) => {
        setQueue(q)
        setLoadError(undefined)
      })
      .catch((e) => setLoadError(e.message))
  }, [])

  const [reviews, setReviews] = useState<ReviewsResponse>({ reviews: [], workerConnected: true })
  const [reviewOptions, setReviewOptions] = useState<ReviewOptions>()
  const [reviewsError, setReviewsError] = useState<string>()
  const loadReviews = useCallback(() => {
    getReviews()
      .then((r) => {
        setReviews(r)
        setReviewsError(undefined)
      })
      .catch((e) => setReviewsError(e.message))
  }, [])
  // On a progress report, reload only the active reviews: the finished ones have long results.
  const loadActiveReviews = useCallback(() => {
    getReviews(true)
      .then((active) => {
        const byId = new Map(active.reviews.map((r) => [r.id, r]))
        setReviews((prev) => ({
          workerConnected: active.workerConnected,
          reviews: prev.reviews.map((r) => byId.get(r.id) ?? r),
        }))
        setReviewsError(undefined)
      })
      .catch((e) => setReviewsError(e.message))
  }, [])
  // Called on every (re)connection, so a failure at page load is retried once the api is back.
  const loadReviewOptions = useCallback(() => {
    getReviewOptions()
      .then((o) => {
        setReviewOptions(o)
        setReviewsError(undefined)
      })
      .catch((e) => setReviewsError(e.message))
  }, [])
  // The API sends the newest reviews first: keep the first one seen for each PR.
  const latestReviews = useMemo(() => {
    const m = new Map<string, Review>()
    for (const r of reviews.reviews) if (!m.has(r.prId)) m.set(r.prId, r)
    return m
  }, [reviews])
  // No event comes when the worker goes missing: while a review waits for it, check every 10s.
  const waiting = reviews.reviews.some((r) => r.status === 'queued' || r.status === 'preparing')
  useEffect(() => {
    if (!waiting) return
    const id = setInterval(loadReviews, 10_000)
    return () => clearInterval(id)
  }, [waiting, loadReviews])

  const refresh = () => {
    setRefreshing(true)
    refreshQueue()
      .then((q) => {
        setQueue(q)
        setLoadError(undefined)
      })
      .catch((e) => setLoadError(e.message))
      .finally(() => setRefreshing(false))
  }

  // Reload every minute too, so "Checked ... ago" and the other times stay current: a poll
  // that finds no change sends no event.
  useEffect(() => {
    const id = setInterval(load, 60_000)
    return () => clearInterval(id)
  }, [load])

  const health = useHealth()
  const live = useLiveUpdates((type) => {
    // resync: the api's database listener reconnected, so events may have been missed.
    if (type === 'connected' || type === 'queue.changed' || type === 'resync') load()
    if (type === 'connected' || type === 'resync') loadReviewOptions()
    if (type === 'connected' || type === 'review.changed' || type === 'resync') loadReviews()
    if (type === 'review.progress') loadActiveReviews()
  })

  return (
    <>
      <header>
        <h1>next-gen-review</h1>
        <div className="status">
          <Status label="API and database" state={health} />
          <Status label="Live updates" state={live} />
        </div>
      </header>
      <main>
        {reviewsError && <p className="error">Could not load the reviews: {reviewsError}</p>}
        <Queue
          queue={queue}
          reviewOptions={reviewOptions}
          latestReviews={latestReviews}
          workerConnected={reviews.workerConnected}
          loadError={loadError}
          refreshing={refreshing}
          onRefresh={refresh}
        />
        <WatchedRepos />
      </main>
    </>
  )
}
