// Fakes for the browser APIs the page uses: fetch, answered by path, and WebSocket.
import { vi } from 'vitest'

type Answer = { status?: number; body?: unknown }

// fakeFetch answers each request with routes[path], or 404. A function route is called per
// request, so a test can change the answer, or hold it with a promise. calls lists
// "METHOD path" in order.
export function fakeFetch(routes: Record<string, Answer | (() => Answer | Promise<Answer>)>) {
  const calls: string[] = []
  const fetch = vi.fn(async (path: string, init?: RequestInit) => {
    calls.push(`${init?.method ?? 'GET'} ${path}`)
    const route = routes[path]
    const { status = 200, body } = (await (typeof route === 'function' ? route() : route)) ?? { status: 404 }
    return new Response(status === 204 ? null : JSON.stringify(body ?? {}), {
      status,
      statusText: status === 200 ? 'OK' : 'Error',
      headers: { 'Content-Type': 'application/json' },
    })
  })
  vi.stubGlobal('fetch', fetch)
  return { fetch, calls }
}

// FakeWebSocket records the sockets the page opens; tests call their handlers.
export class FakeWebSocket {
  static sockets: FakeWebSocket[] = []
  onopen?: () => void
  onmessage?: (msg: { data: string }) => void
  onclose?: () => void
  closed = false
  url: string
  constructor(url: string) {
    this.url = url
    FakeWebSocket.sockets.push(this)
  }
  close() {
    this.closed = true
    this.onclose?.()
  }
  // send delivers a server event to the page.
  send(type: string) {
    this.onmessage?.({ data: JSON.stringify({ type }) })
  }
  static install() {
    FakeWebSocket.sockets = []
    vi.stubGlobal('WebSocket', FakeWebSocket)
  }
  static get last() {
    return FakeWebSocket.sockets[FakeWebSocket.sockets.length - 1]
  }
}
