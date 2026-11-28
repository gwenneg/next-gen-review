import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { fakeFetch } from './test/fakes'
import { WatchedRepos } from './WatchedRepos'

describe('WatchedRepos', () => {
  it('adds and removes repos', async () => {
    let repos = ['o/a']
    const { calls } = fakeFetch({
      '/api/watched-repos': () => {
        if (calls.at(-1)?.startsWith('POST')) repos = [...repos, 'o/b']
        return { status: calls.at(-1)?.startsWith('POST') ? 201 : 200, body: { repos, repo: 'o/b' } }
      },
      '/api/watched-repos/o/a': () => {
        repos = repos.filter((r) => r !== 'o/a')
        return { status: 204 }
      },
    })
    render(<WatchedRepos reload={1} />)
    expect(await screen.findByText('o/a')).toBeTruthy()
    await userEvent.type(screen.getByLabelText('Repo to watch'), '  o/b ')
    await userEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(await screen.findByText('o/b')).toBeTruthy()
    expect((screen.getByLabelText('Repo to watch') as HTMLInputElement).value).toBe('')
    await userEvent.click(screen.getByRole('button', { name: 'Remove o/a' }))
    await waitFor(() => expect(screen.queryByText('o/a')).toBeNull())
    expect(calls).toContain('POST /api/watched-repos')
    expect(calls).toContain('DELETE /api/watched-repos/o/a')
  })

  it("shows the api's refusal, until the next action", async () => {
    const { calls } = fakeFetch({
      '/api/watched-repos': () =>
        calls.at(-1)?.startsWith('POST')
          ? { status: 400, body: { error: 'o/private is private: only public repos can be watched' } }
          : { body: { repos: [] } },
    })
    const { rerender } = render(<WatchedRepos reload={1} />)
    await userEvent.type(screen.getByLabelText('Repo to watch'), 'o/private')
    await userEvent.click(screen.getByRole('button', { name: 'Add' }))
    expect(await screen.findByText(/only public repos/)).toBeTruthy()
    // A reload after a reconnection doesn't hide it before it's read.
    rerender(<WatchedRepos reload={2} />)
    await waitFor(() => expect(calls.filter((c) => c === 'GET /api/watched-repos')).toHaveLength(2))
    expect(screen.getByText(/only public repos/)).toBeTruthy()
  })

  it('loads only when the page says so: once per connection', () => {
    const { calls } = fakeFetch({ '/api/watched-repos': { body: { repos: [] } } })
    render(<WatchedRepos reload={0} />)
    expect(calls).toEqual([])
  })

  // A reload sent before a remove answers after the remove's own reload: its old list is ignored.
  it('ignores a load that answers after a newer one', async () => {
    let release = () => {}
    let first = true
    let repos = ['o/a']
    fakeFetch({
      '/api/watched-repos': () => {
        if (!first) return { body: { repos } }
        first = false
        const old = { body: { repos: [...repos] } }
        return new Promise((resolve) => (release = () => resolve(old)))
      },
      '/api/watched-repos/o/a': () => {
        repos = []
        return { status: 204 }
      },
    })
    const { rerender } = render(<WatchedRepos reload={1} />)
    // The first load hangs; a second one shows the list, and the remove goes through.
    await act(async () => rerender(<WatchedRepos reload={2} />))
    await userEvent.click(await screen.findByRole('button', { name: 'Remove o/a' }))
    await waitFor(() => expect(screen.queryByText('o/a')).toBeNull())
    await act(async () => release())
    expect(screen.queryByText('o/a')).toBeNull()
  })

  // The page loaded while the api was down: the list comes back with the live updates.
  it('loads again on reload, and clears the old error', async () => {
    let up = false
    fakeFetch({ '/api/watched-repos': () => (up ? { body: { repos: ['o/a'] } } : { status: 502, body: { error: '502 Bad Gateway' } }) })
    const { rerender } = render(<WatchedRepos reload={1} />)
    expect(await screen.findByText('Could not load the watched repos: 502 Bad Gateway')).toBeTruthy()
    up = true
    rerender(<WatchedRepos reload={2} />)
    expect(await screen.findByText('o/a')).toBeTruthy()
    expect(screen.queryByText(/502 Bad Gateway/)).toBeNull()
  })
})
