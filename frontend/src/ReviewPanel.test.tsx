import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { ReviewPanel } from './ReviewPanel'
import { fakeFetch } from './test/fakes'
import { options, pr, review } from './test/data'

const warning = /worker isn't connected/

describe('ReviewPanel', () => {
  it('starts a review with the chosen options', async () => {
    const { fetch } = fakeFetch({ '/api/reviews': { status: 201, body: review({ status: 'queued' }) } })
    render(<ReviewPanel pr={pr} options={options} workerConnected />)
    await userEvent.click(screen.getByRole('button', { name: 'Review' }))
    await userEvent.selectOptions(screen.getByLabelText('Model'), 'haiku')
    await userEvent.selectOptions(screen.getByLabelText('Effort'), 'low')
    await userEvent.clear(screen.getByLabelText('Token cap'))
    await userEvent.type(screen.getByLabelText('Token cap'), '20000')
    await userEvent.type(screen.getByLabelText(/Context for the AI/), 'focus on retries')
    await userEvent.click(screen.getByRole('button', { name: 'Start the review' }))
    await waitFor(() => expect(screen.queryByRole('button', { name: 'Start the review' })).toBeNull())
    expect(JSON.parse(fetch.mock.calls[0][1]!.body as string)).toEqual({
      prId: 'PR_1',
      model: 'haiku',
      effort: 'low',
      tokenCap: 20000,
      context: 'focus on retries',
    })
  })

  it("shows the api's error and keeps the form open", async () => {
    fakeFetch({ '/api/reviews': { status: 409, body: { error: 'this PR already has a review queued or running' } } })
    render(<ReviewPanel pr={pr} options={options} workerConnected />)
    await userEvent.click(screen.getByRole('button', { name: 'Review' }))
    await userEvent.click(screen.getByRole('button', { name: 'Start the review' }))
    expect(await screen.findByText('this PR already has a review queued or running')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Start the review' })).toBeTruthy()
  })

  it('offers no review until the options are loaded', () => {
    render(<ReviewPanel pr={pr} workerConnected />)
    expect(screen.queryByRole('button', { name: /Review/ })).toBeNull()
  })

  it.each([
    ['queued', false, true],
    ['preparing', false, true],
    ['running', false, false], // the worker reports on it: it's there
    ['queued', true, false],
  ] as const)('warns about a missing worker: %s, connected %s', (status, workerConnected, shown) => {
    render(<ReviewPanel pr={pr} options={options} latest={review({ status })} workerConnected={workerConnected} />)
    expect(screen.queryByText(warning) !== null).toBe(shown)
  })

  it('cancels an active review, once', async () => {
    const { calls } = fakeFetch({ '/api/reviews/1/cancel': { status: 204 } })
    const { rerender } = render(<ReviewPanel pr={pr} options={options} latest={review()} workerConnected />)
    expect(screen.queryByRole('button', { name: /Review/ })).toBeNull()
    expect(screen.getByText(/1,234 \/ 500,000 tokens/)).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Cancel' }))
    expect(calls).toEqual(['POST /api/reviews/1/cancel'])
    rerender(<ReviewPanel pr={pr} options={options} latest={review({ cancelRequested: true })} workerConnected />)
    expect((screen.getByRole('button', { name: 'Canceling…' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('shows a finished review as Markdown, with its error', () => {
    const done = review({ status: 'capped', result: '## Findings\n\nNone.', error: 'stopped at the token cap of 500000', headSha: 'old' })
    render(<ReviewPanel pr={pr} options={options} latest={done} workerConnected />)
    expect(screen.getByRole('heading', { name: 'Findings' })).toBeTruthy()
    expect(screen.getByText('stopped at the token cap of 500000')).toBeTruthy()
    expect(screen.getByText(/Stopped at the token cap/)).toBeTruthy()
    expect(screen.getByText(/on an older commit/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Review again' })).toBeTruthy()
  })
})
