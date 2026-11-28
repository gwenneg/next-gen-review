import { useEffect, useRef, useState, type FormEvent } from 'react'
import { addWatchedRepo, getWatchedRepos, removeWatchedRepo } from './api'

// Lists the watched repos, with a form to add one and a button to remove each. It loads them each
// time reload changes, from 1: the page passes the number of live-update connections, and loads
// everything on each, so a list that failed to load while the api was down comes back with it.
export function WatchedRepos({ reload }: { reload: number }) {
  const [repos, setRepos] = useState<string[]>([])
  const [input, setInput] = useState('')
  const [loadError, setLoadError] = useState<string>()
  const [actionError, setActionError] = useState<string>()
  const [busy, setBusy] = useState(false)
  // Only the latest load's answer is used: an older one may predate an add or remove.
  const latest = useRef(0)

  const load = () => {
    const n = ++latest.current
    return getWatchedRepos()
      .then((r) => {
        if (n !== latest.current) return
        setRepos(r)
        setLoadError(undefined)
      })
      .catch((e) => {
        if (n === latest.current) setLoadError(e.message)
      })
  }
  useEffect(() => {
    if (reload > 0) load()
  }, [reload])

  const run = (action: Promise<unknown>) => {
    setBusy(true)
    setActionError(undefined)
    return action
      .then(load)
      .catch((e) => setActionError(e.message))
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
      {loadError && <p className="error">Could not load the watched repos: {loadError}</p>}
      {actionError && <p className="error">{actionError}</p>}
    </section>
  )
}
