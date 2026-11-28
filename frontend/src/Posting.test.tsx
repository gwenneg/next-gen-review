import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { Posting } from './api'
import { PostingPanel } from './Posting'
import { fakeFetch } from './test/fakes'
import { review } from './test/data'

const drafts = (over: Partial<Posting> = {}): Posting => ({
  main: 'Please fix the retry.',
  comments: [
    { id: 1, path: 'a.go', line: 4, side: 'RIGHT', body: '[High] Retries forever.', inDiff: true },
    { id: 2, path: 'b.go', line: 9, startLine: 7, side: 'LEFT', body: '[Low] Rename.', inDiff: false },
  ],
  posted: false,
  postedUrl: '',
  postedEvent: '',
  postingEnabled: true,
  postsAs: 'maintainer',
  ...over,
})

const done = review({ id: 3, status: 'done', headSha: 'abc' })

describe('PostingPanel', () => {
  it('shows the drafts, and which ones go in the main comment', async () => {
    fakeFetch({ '/api/reviews/3/posting': { body: drafts() } })
    render(<PostingPanel review={done} headSha="abc" />)
    expect(((await screen.findByLabelText('Main comment')) as HTMLTextAreaElement).value).toBe('Please fix the retry.')
    expect((screen.getByLabelText('Comment on a.go:4') as HTMLTextAreaElement).value).toBe('[High] Retries forever.')
    expect(screen.getByText(/Not on a line of the diff: posted in the main comment/)).toBeTruthy()
    expect(screen.getByLabelText('Comment on b.go:7-9 (old code)')).toBeTruthy()
    expect(screen.queryByText(/pushed commits since this review/)).toBeNull()
  })

  it('saves an edit when the field loses focus, and only if it changed', async () => {
    const { calls, fetch } = fakeFetch({
      '/api/reviews/3/posting': { body: drafts() },
      '/api/reviews/3/posting/main': { body: drafts({ main: 'Fix it.' }) },
      '/api/reviews/3/posting/comments/1': { body: drafts() },
    })
    render(<PostingPanel review={done} headSha="abc" />)
    const main = await screen.findByLabelText('Main comment')
    await userEvent.click(main)
    await userEvent.tab() // unchanged: nothing saved
    await userEvent.clear(main)
    await userEvent.type(main, 'Fix it.')
    await userEvent.tab()
    await waitFor(() => expect(calls).toContain('PUT /api/reviews/3/posting/main'))
    expect(calls.filter((c) => c.startsWith('PUT'))).toHaveLength(1)
    const put = fetch.mock.calls.find(([path]) => path === '/api/reviews/3/posting/main')!
    expect(JSON.parse(put[1]!.body as string)).toEqual({ body: 'Fix it.' })
  })

  it('adds and deletes comments', async () => {
    const { calls, fetch } = fakeFetch({
      '/api/reviews/3/posting': { body: drafts() },
      '/api/reviews/3/posting/comments': { body: drafts() },
      '/api/reviews/3/posting/comments/2': { body: drafts({ comments: drafts().comments.slice(0, 1) }) },
    })
    render(<PostingPanel review={done} headSha="abc" />)
    await userEvent.type(await screen.findByLabelText('File'), 'c.go')
    await userEvent.type(screen.getByLabelText('Line'), '12')
    await userEvent.selectOptions(screen.getByLabelText('Side'), 'LEFT')
    await userEvent.type(screen.getByLabelText('New comment'), 'Why?')
    await userEvent.click(screen.getByRole('button', { name: 'Add a comment' }))
    await waitFor(() => expect(calls).toContain('POST /api/reviews/3/posting/comments'))
    const post = fetch.mock.calls.find(([path]) => path === '/api/reviews/3/posting/comments')!
    expect(JSON.parse(post[1]!.body as string)).toEqual({ path: 'c.go', line: 12, side: 'LEFT', body: 'Why?' })

    await userEvent.click(screen.getByRole('button', { name: 'Delete the comment on b.go:7-9 (old code)' }))
    await waitFor(() => expect(screen.queryByLabelText('Comment on b.go:7-9 (old code)')).toBeNull())
    expect(calls).toContain('DELETE /api/reviews/3/posting/comments/2')
  })

  it('posts with the chosen verdict after a confirmation, then shows the review', async () => {
    const { calls, fetch } = fakeFetch({
      '/api/reviews/3/posting': { body: drafts() },
      '/api/reviews/3/posting/post': {
        body: drafts({ posted: true, postedUrl: 'https://github.com/o/r/pull/7#pullrequestreview-1', postedEvent: 'APPROVE' }),
      },
    })
    render(<PostingPanel review={done} headSha="abc" />)
    await userEvent.selectOptions(await screen.findByLabelText('Verdict'), 'APPROVE')
    await userEvent.click(screen.getByRole('button', { name: 'Post to GitHub…' }))
    expect(calls).not.toContain('POST /api/reviews/3/posting/post')
    expect(screen.getByText(/Post 1 inline comment and the main comment \(with 1 comment outside the diff\) to o\/r#7/)).toBeTruthy()
    expect(screen.getByText(/as maintainer\?/)).toBeTruthy()
    await userEvent.click(screen.getByRole('button', { name: 'Post now' }))
    expect((await screen.findByRole('link', { name: 'the review' })).getAttribute('href')).toBe(
      'https://github.com/o/r/pull/7#pullrequestreview-1',
    )
    expect(screen.getByText(/Posted to GitHub as Approve/)).toBeTruthy()
    const post = fetch.mock.calls.find(([path]) => path === '/api/reviews/3/posting/post')!
    expect(JSON.parse(post[1]!.body as string)).toEqual({ event: 'APPROVE' })
    expect(screen.queryByLabelText('Main comment')).toBeNull() // posted comments can't change
  })

  it('can go back from the confirmation', async () => {
    const { calls } = fakeFetch({ '/api/reviews/3/posting': { body: drafts() } })
    render(<PostingPanel review={done} headSha="abc" />)
    await userEvent.click(await screen.findByRole('button', { name: 'Post to GitHub…' }))
    await userEvent.click(screen.getByRole('button', { name: 'Back' }))
    expect(screen.getByRole('button', { name: 'Post to GitHub…' })).toBeTruthy()
    expect(calls).toEqual(['GET /api/reviews/3/posting'])
  })

  it("shows GitHub's refusal, and stays ready to post again", async () => {
    fakeFetch({
      '/api/reviews/3/posting': { body: drafts() },
      '/api/reviews/3/posting/post': { status: 502, body: { error: 'GitHub refused the review: Can not approve your own pull request' } },
    })
    render(<PostingPanel review={done} headSha="abc" />)
    await userEvent.click(await screen.findByRole('button', { name: 'Post to GitHub…' }))
    await userEvent.click(screen.getByRole('button', { name: 'Post now' }))
    expect(await screen.findByText(/Can not approve your own pull request/)).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Post now' })).toBeTruthy()
  })

  it('keeps a new comment when adding it fails', async () => {
    fakeFetch({
      '/api/reviews/3/posting': { body: drafts() },
      '/api/reviews/3/posting/comments': { status: 409, body: { error: "this review's comments were already posted to GitHub" } },
    })
    render(<PostingPanel review={done} headSha="abc" />)
    await userEvent.type(await screen.findByLabelText('File'), 'c.go')
    await userEvent.type(screen.getByLabelText('Line'), '12')
    await userEvent.type(screen.getByLabelText('New comment'), 'A long comment')
    await userEvent.click(screen.getByRole('button', { name: 'Add a comment' }))
    expect(await screen.findByText(/already posted/)).toBeTruthy()
    expect((screen.getByLabelText('New comment') as HTMLTextAreaElement).value).toBe('A long comment')
    expect((screen.getByLabelText('Line') as HTMLInputElement).value).toBe('12')
  })

  it("says when a posted review's link wasn't recorded", async () => {
    fakeFetch({ '/api/reviews/3/posting': { body: drafts({ posted: true, postedEvent: 'COMMENT' }) } })
    render(<PostingPanel review={done} headSha="abc" />)
    expect(await screen.findByText(/Posted to GitHub as Comment\. Its link wasn’t recorded/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Post to GitHub…' })).toBeNull()
  })

  it('warns when the PR changed since the review, and when posting is off', async () => {
    fakeFetch({ '/api/reviews/3/posting': { body: drafts({ postingEnabled: false }) } })
    render(<PostingPanel review={done} headSha="newer" />)
    expect(await screen.findByText(/pushed commits since this review/)).toBeTruthy()
    expect(screen.getByText(/Posting is off/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Post to GitHub…' })).toBeNull()
  })
})
