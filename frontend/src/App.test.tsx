import { act, render, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import App from './App'
import type { Review } from './api'
import { FakeWebSocket, fakeFetch } from './test/fakes'
import { options, pr, review } from './test/data'

type Routes = Parameters<typeof fakeFetch>[0]

// start renders the page with these api answers, and connects its live updates.
async function start(routes: Routes) {
  FakeWebSocket.install()
  const f = fakeFetch({
    '/api/health': { body: {} },
    '/api/queue': { body: { items: [pr], status: {}, tokenSource: 'default' } },
    '/api/watched-repos': { body: { repos: [] } },
    '/api/review-options': { body: options },
    '/api/reviews': { body: { reviews: [], workerConnected: true } },
    ...routes,
  })
  render(<App />)
  await act(async () => FakeWebSocket.last.onopen!())
  return f
}

const reviews = (list: Review[], workerConnected = true) => ({ body: { reviews: list, workerConnected } })

describe('App', () => {
  it('loads the queue, reviews and options once live updates connect', async () => {
    const { calls } = await start({})
    expect(await screen.findByRole('button', { name: 'Review' })).toBeTruthy()
    for (const path of ['/api/queue', '/api/reviews', '/api/review-options', '/api/watched-repos']) {
      expect(calls).toContain(`GET ${path}`)
    }
  })

  // The page opened while the api was restarting, or the options request failed alone: the
  // error stays shown, and the Review buttons come back with the next connection or reviews load.
  it.each(['resync', 'review.changed'])('loads the options again on %s, and keeps their error until then', async (event) => {
    let up = false
    await start({ '/api/review-options': () => (up ? { body: options } : { status: 502, body: { error: 'Bad Gateway' } }) })
    expect(await screen.findByText('Could not load the review options: Bad Gateway')).toBeTruthy()
    expect(screen.queryByText(/Could not load the (queue|reviews)/)).toBeNull()
    expect(screen.queryByRole('button', { name: 'Review' })).toBeNull()
    up = true
    await act(async () => FakeWebSocket.last.send(event))
    expect(await screen.findByRole('button', { name: 'Review' })).toBeTruthy()
    expect(screen.queryByText(/Could not load the review options/)).toBeNull()
  })

  // Finished reviews carry long results: a progress report only reloads the active ones.
  it('updates a running review from the active reviews only', async () => {
    const done = review({ id: 2, prId: 'PR_other', status: 'done', result: 'long' })
    const { calls } = await start({
      '/api/reviews': reviews([review({ tokensUsed: 100 }), done]),
      '/api/reviews?active=true': reviews([review({ tokensUsed: 900 })]),
    })
    expect(await screen.findByText(/100 \/ 500,000 tokens/)).toBeTruthy()
    await act(async () => FakeWebSocket.last.send('review.progress'))
    expect(await screen.findByText(/900 \/ 500,000 tokens/)).toBeTruthy()
    expect(calls.filter((c) => c.startsWith('GET /api/reviews'))).toEqual(['GET /api/reviews', 'GET /api/reviews?active=true'])
  })

  // No event comes when the worker goes missing or comes back: a waiting review checks every 10s.
  it('checks every 10s while a review waits for the worker', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    let connected = false
    await start({ '/api/reviews': () => reviews([review({ status: 'queued' })], connected) })
    expect(await screen.findByText(/worker isn't connected/)).toBeTruthy()
    connected = true
    await act(async () => vi.advanceTimersByTime(10_000))
    await waitFor(() => expect(screen.queryByText(/worker isn't connected/)).toBeNull())
  })

  it('loads the watched repos again on each connection', async () => {
    let repos: string[] = []
    await start({ '/api/watched-repos': () => ({ body: { repos } }) })
    repos = ['o/new']
    await act(async () => FakeWebSocket.last.send('resync'))
    expect(await screen.findByText('o/new')).toBeTruthy()
  })
})
