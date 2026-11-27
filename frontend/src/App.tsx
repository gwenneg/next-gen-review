import { useCallback, useEffect, useState } from 'react'
import { getQueue, refreshQueue, type QueueResponse } from './api'
import { useLiveUpdates, type LiveState } from './live'
import { Queue } from './Queue'
import { WatchedRepos } from './WatchedRepos'

// Polls the API health endpoint, which also reports whether the database is reachable.
function useHealth(): LiveState {
  const [state, setState] = useState<LiveState>('connecting')
  useEffect(() => {
    const check = () =>
      fetch('/api/health')
        .then((r) => setState(r.ok ? 'ok' : 'bad'))
        .catch(() => setState('bad'))
    check()
    const id = setInterval(check, 10_000)
    return () => clearInterval(id)
  }, [])
  return state
}

function Status({ label, state }: { label: string; state: LiveState }) {
  return (
    <span title={state}>
      <span className={`dot ${state === 'connecting' ? '' : state}`} />
      {label}
    </span>
  )
}

export default function App() {
  const [queue, setQueue] = useState<QueueResponse>()
  const [loadError, setLoadError] = useState<string>()
  const [refreshing, setRefreshing] = useState(false)

  const load = useCallback(() => {
    getQueue()
      .then((q) => {
        setQueue(q)
        setLoadError(undefined)
      })
      .catch((e) => setLoadError(e.message))
  }, [])

  const refresh = () => {
    setRefreshing(true)
    refreshQueue()
      .then((q) => {
        setQueue(q)
        setLoadError(undefined)
      })
      .catch((e) => setLoadError(e.message))
      .finally(() => setRefreshing(false))
  }

  // Reload every minute too, so "Checked ... ago" and the other times stay current: a poll
  // that finds no change sends no event.
  useEffect(() => {
    const id = setInterval(load, 60_000)
    return () => clearInterval(id)
  }, [load])

  const health = useHealth()
  const live = useLiveUpdates((type) => {
    // resync: the api's database listener reconnected, so events may have been missed.
    if (type === 'connected' || type === 'queue.changed' || type === 'resync') load()
  })

  return (
    <>
      <header>
        <h1>next-gen-review</h1>
        <div className="status">
          <Status label="API and database" state={health} />
          <Status label="Live updates" state={live} />
        </div>
      </header>
      <main>
        <Queue queue={queue} loadError={loadError} refreshing={refreshing} onRefresh={refresh} />
        <WatchedRepos />
      </main>
    </>
  )
}
