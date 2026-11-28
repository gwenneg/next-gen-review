// Types and calls for the Go api. Keep in sync with backend/internal/github/queue.go and internal/queue.

export type Reason =
  | { kind: 'requested'; viaTeam?: boolean; teams?: string[] }
  | { kind: 'watched' }
  | { kind: 'new_commits' }

export type PullRequest = {
  id: string
  repo: string
  number: number
  title: string
  url: string
  author: string
  isDraft: boolean
  createdAt: string
  updatedAt: string
  additions: number
  deletions: number
  changedFiles: number
  headSha: string
  ciState?: 'SUCCESS' | 'FAILURE' | 'PENDING' | 'ERROR' | 'EXPECTED'
  reasons: Reason[]
}

export type QueueResponse = {
  items: PullRequest[]
  status: { refreshedAt?: string; error?: string }
  tokenSource: string // the variable the GitHub token came from, or "default" when there is none
}

async function call<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(path, {
    method,
    headers: body ? { 'Content-Type': 'application/json' } : undefined,
    body: body ? JSON.stringify(body) : undefined,
  })
  if (!res.ok) {
    const err = await res.json().catch(() => ({}))
    throw new Error(err.error ?? `${res.status} ${res.statusText}`)
  }
  return res.status === 204 ? (undefined as T) : res.json()
}

export const getQueue = () => call<QueueResponse>('GET', '/api/queue')
export const refreshQueue = () => call<QueueResponse>('POST', '/api/queue/refresh')
export const getWatchedRepos = () => call<{ repos: string[] }>('GET', '/api/watched-repos').then((r) => r.repos)
export const addWatchedRepo = (repo: string) => call<{ repo: string }>('POST', '/api/watched-repos', { repo })
export const removeWatchedRepo = (repo: string) => call<void>('DELETE', `/api/watched-repos/${repo}`)

// Reviews. Keep in sync with backend/internal/review/review.go.

export type ReviewOptions = {
  models: { value: string; label: string }[]
  efforts: string[]
  defaultModel: string
  defaultEffort: string
  defaultTokenCap: number
  minTokenCap: number
  maxTokenCap: number
  maxContextLength: number
}

export type ReviewStatus = 'queued' | 'preparing' | 'running' | 'done' | 'failed' | 'canceled' | 'capped'

export type Review = {
  id: number
  prId: string
  repo: string
  number: number
  headSha: string
  model: string
  effort: string
  tokenCap: number
  context: string
  status: ReviewStatus
  cancelRequested: boolean
  sessionId: string
  tokensUsed: number
  result: string
  error: string
  createdAt: string
  startedAt: string | null
  finishedAt: string | null
}

export type ReviewRequest = { prId: string; model: string; effort: string; tokenCap: number; context: string }

export const isActive = (r: Review) => ['queued', 'preparing', 'running'].includes(r.status)

export const getReviewOptions = () => call<ReviewOptions>('GET', '/api/review-options')
// workerConnected is false when the worker hasn't called the api lately: queued reviews won't start.
export type ReviewsResponse = { reviews: Review[]; workerConnected: boolean }
export const getReviews = (activeOnly = false) =>
  call<ReviewsResponse>('GET', activeOnly ? '/api/reviews?active=true' : '/api/reviews')
export const startReview = (req: ReviewRequest) => call<Review>('POST', '/api/reviews', req)
export const cancelReview = (id: number) => call<void>('POST', `/api/reviews/${id}/cancel`)
