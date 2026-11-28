import { describe, expect, it } from 'vitest'
import { cancelReview, getReviews, startReview } from './api'
import { fakeFetch } from './test/fakes'

describe('api', () => {
  it("throws the api's error message", async () => {
    fakeFetch({ '/api/reviews': { status: 409, body: { error: 'this PR already has a review queued or running' } } })
    await expect(startReview({ prId: 'PR_1', model: 'sonnet', effort: 'low', tokenCap: 1, context: '' })).rejects.toThrow(
      'this PR already has a review queued or running',
    )
  })

  it('throws the status when the error is not JSON, as from nginx', async () => {
    const { fetch } = fakeFetch({})
    fetch.mockResolvedValueOnce(new Response('<html>502</html>', { status: 502, statusText: 'Bad Gateway' }))
    await expect(getReviews()).rejects.toThrow('502 Bad Gateway')
  })

  it('returns nothing for 204, and asks for active reviews only when told', async () => {
    const { calls } = fakeFetch({
      '/api/reviews/3/cancel': { status: 204 },
      '/api/reviews?active=true': { body: { reviews: [], workerConnected: true } },
    })
    await expect(cancelReview(3)).resolves.toBeUndefined()
    await getReviews(true)
    expect(calls).toEqual(['POST /api/reviews/3/cancel', 'GET /api/reviews?active=true'])
  })
})
