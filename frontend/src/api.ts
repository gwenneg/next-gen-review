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
