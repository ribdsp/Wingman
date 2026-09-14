'use client'

// Talking to this app's own /api routes from the browser.
//
// The browser never sees a service address and never holds a credential: it asks the
// console, the console forwards what its table allows. What comes back is the same envelope
// the services use, so the parser is the same one — parseEnvelope, shared with the server
// side. Two parsers for one shape would eventually disagree.
//
// The polling hook is here rather than in each panel because everything on this deck is a
// poll: core has no streaming endpoint, deliberately, so "what is true now" is a question
// asked on an interval.

import { useCallback, useEffect, useRef, useState } from 'react'
import { ApiError, ERR_MALFORMED, type Parsed, parseEnvelope } from './envelope'

export type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'

/**
 * call performs one request against the console's own API.
 *
 * `credentials: 'same-origin'` is explicit rather than relied upon: both cookies are
 * SameSite=Strict and httpOnly, and this is the only way they are ever attached.
 */
export async function call<T>(
  path: string,
  method: Method = 'GET',
  body?: unknown,
  signal?: AbortSignal,
): Promise<Parsed<T>> {
  const headers: Record<string, string> = { Accept: 'application/json' }
  if (body !== undefined) headers['Content-Type'] = 'application/json'

  const response = await fetch(path, {
    method,
    headers,
    body: body === undefined ? undefined : JSON.stringify(body),
    credentials: 'same-origin',
    cache: 'no-store',
    ...(signal ? { signal } : {}),
  })

  const text = await response.text()
  let parsedBody: unknown = null
  if (text !== '') {
    try {
      parsedBody = JSON.parse(text) as unknown
    } catch {
      throw new ApiError({
        status: response.status,
        code: ERR_MALFORMED,
        message: 'The console answered with something that was not JSON.',
      })
    }
  }
  return parseEnvelope<T>(response.status, parsedBody)
}

/** get is a read. Returns the whole parsed envelope, because lists need the pagination. */
export function get<T>(path: string, signal?: AbortSignal): Promise<Parsed<T>> {
  return call<T>(path, 'GET', undefined, signal)
}

/** message turns any thrown thing into one line a person can act on. */
export function message(error: unknown): string {
  if (error instanceof ApiError) return error.message
  if (error instanceof Error && error.name === 'AbortError') return ''
  return 'Something failed in the console.'
}

export type Poll<T> = {
  value: T
  /** Empty when the last read succeeded. */
  error: string
  /** True while a read is in flight, which is what the sweep indicator shows. */
  loading: boolean
  /** Read again now — used after an action that changes what is on screen. */
  refresh: () => void
}

/**
 * usePoll re-reads something on an interval, and keeps the last good value.
 *
 * Three deliberate behaviours:
 *
 *   - The value is not cleared when a read fails. A deck that blanked its numbers because
 *     one poll timed out would be less useful than one showing the last known reading next
 *     to the fact that the last read failed.
 *   - A hidden tab does not poll. An ops console left open on another desktop should not
 *     be a load generator, and it re-reads the moment it becomes visible again.
 *   - The first interval is waited out rather than fired immediately: the server already
 *     rendered this page's data on this request, so an instant re-read would double every
 *     navigation for nothing.
 */
export function usePoll<T>(load: (signal: AbortSignal) => Promise<T>, intervalMs: number, initial: T): Poll<T> {
  const [value, setValue] = useState<T>(initial)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const [nonce, setNonce] = useState(0)

  // The caller passes a fresh closure on every render; keeping it in a ref means the
  // interval is not torn down and rebuilt each time, which would reset its phase.
  const loader = useRef(load)
  useEffect(() => {
    loader.current = load
  }, [load])

  useEffect(() => {
    let cancelled = false
    const controller = new AbortController()

    const run = async () => {
      setLoading(true)
      try {
        const next = await loader.current(controller.signal)
        if (cancelled) return
        setValue(next)
        setError('')
      } catch (thrown) {
        if (cancelled) return
        const text = message(thrown)
        if (text) setError(text)
      } finally {
        if (!cancelled) setLoading(false)
      }
    }

    if (nonce > 0) void run()

    const timer = setInterval(() => {
      if (!document.hidden) void run()
    }, intervalMs)

    const onVisible = () => {
      if (!document.hidden) void run()
    }
    document.addEventListener('visibilitychange', onVisible)

    return () => {
      cancelled = true
      controller.abort()
      clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
    }
  }, [intervalMs, nonce])

  const refresh = useCallback(() => setNonce((n) => n + 1), [])
  return { value, error, loading, refresh }
}

/**
 * useNow ticks a clock for the countdowns — held operator authority, an approval's
 * expiry. Coarse on purpose: a per-second countdown next to a four-second poll implies a
 * precision the screen does not have.
 */
export function useNow(intervalMs = 15_000): number {
  const [now, setNow] = useState(() => Date.now())
  useEffect(() => {
    const timer = setInterval(() => setNow(Date.now()), intervalMs)
    return () => clearInterval(timer)
  }, [intervalMs])
  return now
}
