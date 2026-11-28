import { act, renderHook } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { useLiveUpdates } from './live'
import { FakeWebSocket } from './test/fakes'

describe('useLiveUpdates', () => {
  it('passes events on, and reconnects 2s after the socket closes', () => {
    vi.useFakeTimers()
    FakeWebSocket.install()
    const events: string[] = []
    const { result, unmount } = renderHook(() => useLiveUpdates((type) => events.push(type)))
    expect(result.current).toBe('connecting')
    const ws = FakeWebSocket.last
    expect(ws.url).toBe('ws://localhost:3000/api/ws')

    act(() => ws.onopen!())
    act(() => ws.send('hello')) // the server's greeting isn't an event
    act(() => ws.send('queue.changed'))
    expect(result.current).toBe('ok')
    expect(events).toEqual(['connected', 'queue.changed'])

    act(() => ws.onclose!())
    expect(result.current).toBe('bad')
    act(() => vi.advanceTimersByTime(1_999))
    expect(FakeWebSocket.sockets).toHaveLength(1)
    act(() => vi.advanceTimersByTime(1))
    expect(FakeWebSocket.sockets).toHaveLength(2)
    act(() => FakeWebSocket.last.onopen!())
    expect(events).toEqual(['connected', 'queue.changed', 'connected'])

    unmount() // closes the socket without reconnecting
    act(() => vi.advanceTimersByTime(10_000))
    expect(FakeWebSocket.last.closed).toBe(true)
    expect(FakeWebSocket.sockets).toHaveLength(2)
  })
})
