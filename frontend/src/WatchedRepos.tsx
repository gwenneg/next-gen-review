import { useEffect, useState, type FormEvent } from 'react'
import { addWatchedRepo, getWatchedRepos, removeWatchedRepo } from './api'

// Lists the watched repos, with a form to add one and a button to remove each.
export function WatchedRepos() {
  const [repos, setRepos] = useState<string[]>([])
  const [input, setInput] = useState('')
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  const load = () =>
    getWatchedRepos()
      .then(setRepos)
      .catch((e) => setError(e.message))
  useEffect(() => {
    load()
  }, [])

  const run = (action: Promise<unknown>) => {
    setBusy(true)
    setError(undefined)
    return action
      .then(load)
      .catch((e) => setError(e.message))
      .finally(() => setBusy(false))
  }

  const add = (e: FormEvent) => {
    e.preventDefault()
    run(addWatchedRepo(input.trim()).then(() => setInput('')))
  }

  return (
    <section>
      <h2>Watched repos</h2>
      <p className="meta">Open PRs in these repos are listed when they aren't drafts and you haven't reviewed them yet.</p>
      <ul className="repos">
        {repos.map((r) => (
          <li key={r}>
            {r}{' '}
            <button onClick={() => run(removeWatchedRepo(r))} disabled={busy} aria-label={`Remove ${r}`}>
              Remove
            </button>
          </li>
        ))}
      </ul>
      <form onSubmit={add}>
        <input value={input} onChange={(e) => setInput(e.target.value)} placeholder="owner/name" aria-label="Repo to watch" />
        <button type="submit" disabled={busy || !input.trim()}>
          Add
        </button>
      </form>
      {error && <p className="error">{error}</p>}
    </section>
  )
}
