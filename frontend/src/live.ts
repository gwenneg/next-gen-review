import { useEffect, useEffectEvent, useState } from 'react'

export type LiveState = 'connecting' | 'ok' | 'bad'

// Keeps a WebSocket open and calls onEvent with each event type. It also calls onEvent with
// 'connected' after every (re)connection, so the page reloads what it may have missed.
export function useLiveUpdates(onEvent: (type: string) => void): LiveState {
  const [state, setState] = useState<LiveState>('connecting')
  const handle = useEffectEvent(onEvent)

  useEffect(() => {
    let ws: WebSocket
    let retry: ReturnType<typeof setTimeout>
    let stopped = false
    const connect = () => {
      const scheme = location.protocol === 'https:' ? 'wss' : 'ws'
      ws = new WebSocket(`${scheme}://${location.host}/api/ws`)
      ws.onopen = () => {
        setState('ok')
        handle('connected')
      }
      ws.onmessage = (msg) => {
        const { type } = JSON.parse(msg.data)
        if (type !== 'hello') handle(type)
      }
      ws.onclose = () => {
        if (stopped) return
        setState('bad')
        retry = setTimeout(connect, 2_000)
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
