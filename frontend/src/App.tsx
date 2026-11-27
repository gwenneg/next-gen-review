import { useEffect, useState } from 'react'

type State = 'connecting' | 'ok' | 'bad'

// Polls the API health endpoint, which also reports whether the database is reachable.
function useHealth(): State {
  const [state, setState] = useState<State>('connecting')
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

// Keeps a WebSocket open for live updates, and reconnects after a drop.
function useLiveUpdates(): State {
  const [state, setState] = useState<State>('connecting')
  useEffect(() => {
    let ws: WebSocket
    let retry: ReturnType<typeof setTimeout>
    let stopped = false
    const connect = () => {
      const scheme = location.protocol === 'https:' ? 'wss' : 'ws'
      ws = new WebSocket(`${scheme}://${location.host}/api/ws`)
      ws.onopen = () => setState('ok')
      ws.onclose = () => {
        setState('bad')
        if (!stopped) retry = setTimeout(connect, 2_000)
      }
    }
    connect()
    return () => {
      stopped = true
      clearTimeout(retry)
      ws.close()
    }
  }, [])
  return state
}

function Status({ label, state }: { label: string; state: State }) {
  return (
    <span title={state}>
      <span className={`dot ${state === 'connecting' ? '' : state}`} />
      {label}
    </span>
  )
}

export default function App() {
  const health = useHealth()
  const live = useLiveUpdates()
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
        <h2>Review queue</h2>
        <p>Nothing here yet.</p>
      </main>
    </>
  )
}
