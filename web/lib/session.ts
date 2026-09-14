// The two cookies this console keeps, and nothing else.
//
// Neither holds anything the browser can use. Both hold a sealed envelope that only this
// server opens (see seal.ts), and both are httpOnly, so the credential inside is never
// in a bundle, never in a response body, and never reachable from a script.
//
// There is no third cookie. In particular there is no "role" or "isOperator" cookie:
// authority here is only ever the presence of an unexpired, openable operator seal, and
// a boolean a client could flip would be a boolean worth flipping.

import { cookies } from 'next/headers'
import { config } from './env'
import { expiryOf, open, seal } from './seal'

export const SESSION_COOKIE = 'wgm_session'
export const OPERATOR_COOKIE = 'wgm_operator'

/**
 * SessionPayload is what a signed-in person's cookie holds.
 *
 * The display fields are copied in so the rail can render who is signed in without a
 * round trip to core on every navigation. They are a convenience; the token is the only
 * part that is authority, and core re-checks it on every call.
 */
export type SessionPayload = {
  token: string
  userId: string
  email: string
  displayName: string
}

/**
 * Cookie attributes, shared by both.
 *
 * sameSite strict rather than lax: every navigation inside this console is same-site, so
 * the only thing strict costs is that arriving from an external link shows the sign-in
 * screen once. That is a small price for a console whose buttons resolve approvals.
 *
 * secure follows NODE_ENV because the documented deployment is behind the operator's own
 * TLS, while `npm run dev` is plain http on loopback. A hardcoded true would make the
 * dev server unusable and invite somebody to remove it permanently.
 */
function attributes(maxAgeSeconds: number) {
  return {
    httpOnly: true,
    sameSite: 'strict' as const,
    secure: process.env.NODE_ENV === 'production',
    path: '/',
    maxAge: maxAgeSeconds,
  }
}

/** readSession returns the signed-in person, or null. */
export async function readSession(now = Date.now()): Promise<SessionPayload | null> {
  const raw = (await cookies()).get(SESSION_COOKIE)?.value
  if (!raw) return null
  const payload = open<SessionPayload>('session', raw, config().cookieKey, now)
  if (!payload || typeof payload.token !== 'string' || !payload.token) return null
  return payload
}

/**
 * writeSession seals a core session token into the cookie.
 *
 * expiresAt comes from core's sign-in response, not from a local constant: core decides
 * how long its own session lives (SESSION_TTL), and a cookie that outlived the token
 * would produce a console that looks signed in and 401s on every read.
 */
export async function writeSession(payload: SessionPayload, expiresAt: number): Promise<void> {
  const now = Date.now()
  const seconds = Math.max(1, Math.floor((expiresAt - now) / 1000))
  const store = await cookies()
  store.set(SESSION_COOKIE, seal('session', payload, expiresAt, config().cookieKey), attributes(seconds))
}

/** clearSession removes the cookie. Called on sign-out and on a 401 from core. */
export async function clearSession(): Promise<void> {
  ;(await cookies()).delete(SESSION_COOKIE)
}

/**
 * readOperatorKey returns the pasted goal-engine operator key, or null.
 *
 * Server-side only, by construction: it needs the cookie key, which is in the server's
 * environment. If this ever appears in a file with 'use client' at the top, the build
 * fails, which is the intended failure mode.
 */
export async function readOperatorKey(now = Date.now()): Promise<string | null> {
  const raw = (await cookies()).get(OPERATOR_COOKIE)?.value
  if (!raw) return null
  const key = open<string>('operator', raw, config().cookieKey, now)
  return typeof key === 'string' && key !== '' ? key : null
}

/**
 * operatorExpiresAt returns when the held authority lapses, without the key itself.
 *
 * This is what the status strip renders. The countdown is deliberately visible: authority
 * you have forgotten you are holding is authority somebody else can use on your laptop.
 */
export async function operatorExpiresAt(now = Date.now()): Promise<number | null> {
  const raw = (await cookies()).get(OPERATOR_COOKIE)?.value
  if (!raw) return null
  return expiryOf('operator', raw, config().cookieKey, now)
}

/**
 * writeOperatorKey seals a pasted key for the configured TTL and returns its expiry.
 *
 * The TTL is short and capped at eight hours by config, and it does not renew on
 * activity. Holding operator authority is meant to be a thing you do and then stop
 * doing, not a state the console drifts into and stays in.
 */
export async function writeOperatorKey(key: string): Promise<number> {
  const ttlMs = config().operatorTtlMinutes * 60_000
  const expiresAt = Date.now() + ttlMs
  const store = await cookies()
  store.set(
    OPERATOR_COOKIE,
    seal('operator', key, expiresAt, config().cookieKey),
    attributes(Math.floor(ttlMs / 1000)),
  )
  return expiresAt
}

/** clearOperatorKey drops the authority immediately, ahead of its expiry. */
export async function clearOperatorKey(): Promise<void> {
  ;(await cookies()).delete(OPERATOR_COOKIE)
}
