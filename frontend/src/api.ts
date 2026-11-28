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
  postedUrl: string // the GitHub review its comments were posted as, or empty
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

// A review's comments for GitHub. Keep in sync with backend/internal/review/drafts.go.

export type Side = 'RIGHT' | 'LEFT'
export type ReviewComment = {
  id: number
  path: string
  line: number
  startLine?: number
  side: Side
  body: string
  inDiff: boolean // GitHub takes it inline; otherwise it's posted in the main comment
}
export type Posting = {
  main: string
  comments: ReviewComment[]
  posted: boolean
  postedUrl: string
  postedEvent: string
  postingEnabled: boolean // GH_REVIEW_TOKEN is set
  postsAs: string // its user
}
export type ReviewEvent = 'COMMENT' | 'APPROVE' | 'REQUEST_CHANGES'
export type NewComment = { path: string; line: number; startLine?: number; side: Side; body: string }

const posting = (id: number, path = '') => `/api/reviews/${id}/posting${path}`
export const getPosting = (id: number) => call<Posting>('GET', posting(id))
export const setMainComment = (id: number, body: string) => call<Posting>('PUT', posting(id, '/main'), { body })
export const addComment = (id: number, c: NewComment) => call<Posting>('POST', posting(id, '/comments'), c)
export const setComment = (id: number, cid: number, body: string) =>
  call<Posting>('PUT', posting(id, `/comments/${cid}`), { body })
export const deleteComment = (id: number, cid: number) => call<Posting>('DELETE', posting(id, `/comments/${cid}`))
export const postReview = (id: number, event: ReviewEvent) => call<Posting>('POST', posting(id, '/post'), { event })
