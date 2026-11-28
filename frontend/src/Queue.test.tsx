import { render, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { Queue } from './Queue'
import { options, pr, review } from './test/data'

const props = { latestReviews: new Map(), workerConnected: true, refreshing: false, onRefresh: () => {} }

describe('Queue', () => {
  it('shows each PR with why it is waiting, and its review', () => {
    const queue = { items: [pr], status: { refreshedAt: new Date().toISOString() }, tokenSource: 'GH_TOKEN' }
    render(<Queue {...props} queue={queue} reviewOptions={options} latestReviews={new Map([['PR_1', review()]])} />)
    expect(screen.getByRole('heading', { name: 'Waiting for you (1)' })).toBeTruthy()
    expect(screen.getByRole('link', { name: 'o/r#7' }).getAttribute('href')).toBe(pr.url)
    expect(screen.getByText(/by octo · opened 3 days ago · updated 2 hours ago/)).toBeTruthy()
    expect(screen.getByText('CI failed')).toBeTruthy()
    expect(screen.getByText('Review requested from a team').getAttribute('title')).toBe('Teams requested on this PR: o/team')
    expect(screen.getByText(/GitHub token from GH_TOKEN · Checked just now/)).toBeTruthy()
    expect(screen.getByText('Running')).toBeTruthy()
  })

  it('says when nothing is waiting, and shows errors', () => {
    const { rerender } = render(<Queue {...props} queue={{ items: [], status: {}, tokenSource: 'default' }} />)
    expect(screen.getByText('Nothing is waiting for you.')).toBeTruthy()
    rerender(<Queue {...props} queue={{ items: [], status: { error: 'GitHub returned 502' }, tokenSource: 'default' }} loadError="conn refused" />)
    expect(screen.queryByText('Nothing is waiting for you.')).toBeNull()
    expect(screen.getByText('Last GitHub check failed: GitHub returned 502')).toBeTruthy()
    expect(screen.getByText('Could not load the queue: conn refused')).toBeTruthy()
  })

  it('refreshes on click, once at a time', () => {
    const onRefresh = vi.fn()
    const { rerender } = render(<Queue {...props} onRefresh={onRefresh} />)
    screen.getByRole('button', { name: 'Refresh' }).click()
    expect(onRefresh).toHaveBeenCalled()
    rerender(<Queue {...props} refreshing />)
    expect((screen.getByRole('button', { name: 'Refreshing…' }) as HTMLButtonElement).disabled).toBe(true)
  })
})
