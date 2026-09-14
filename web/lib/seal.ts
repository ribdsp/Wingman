// Sealed cookie payloads: AES-256-GCM, authenticated, purpose-bound, expiring.
//
// This app keeps two things in cookies — a signed-in person's core session token, and
// an operator's pasted goal-engine key. Both are credentials for another service. A
// plain httpOnly cookie is enough to stop a script reading it, but not enough to stop
// anything that can read the file the browser stores it in, or a proxy that logged the
// header once. So the value in the cookie is not the credential; it is a sealed
// envelope that only this server's key opens.
//
// Hand-rolled rather than a session library: this is one cipher, one layout and one
// expiry check, and node:crypto has all three. A dependency here would be a
// dependency in the trust path.

import { createCipheriv, createDecipheriv, randomBytes, timingSafeEqual } from 'node:crypto'

// v1: [version 1B][iv 12B][tag 16B][ciphertext …], base64url.
const VERSION = 1
const IV_BYTES = 12
const TAG_BYTES = 16

/**
 * Purpose is bound into the seal as additional authenticated data, so a value lifted
 * out of one cookie cannot be replayed into the other. Without this, an operator key
 * and a session token are both "a sealed string" and the server would happily read
 * either as either.
 */
export type Purpose = 'session' | 'operator'

export type Sealed<T> = {
  /** The payload. Whatever was sealed, as it was sealed. */
  value: T
  /** Unix milliseconds. Enforced on open, not merely on the cookie's Max-Age. */
  expiresAt: number
}

/**
 * seal encrypts a payload and returns the cookie value.
 *
 * expiresAt is inside the sealed bytes on purpose. A cookie's Max-Age is a request
 * from the server that the client is free to ignore; an expiry the server checks after
 * decrypting is not.
 */
export function seal<T>(purpose: Purpose, value: T, expiresAt: number, key: Buffer): string {
  if (key.length !== 32) throw new Error('seal: key must be 32 bytes')

  const iv = randomBytes(IV_BYTES)
  const cipher = createCipheriv('aes-256-gcm', key, iv)
  cipher.setAAD(Buffer.from(purpose, 'utf8'))

  const plaintext = Buffer.from(JSON.stringify({ v: value, e: expiresAt }), 'utf8')
  const body = Buffer.concat([cipher.update(plaintext), cipher.final()])
  const tag = cipher.getAuthTag()

  return Buffer.concat([Buffer.from([VERSION]), iv, tag, body]).toString('base64url')
}

/**
 * unseal decrypts a cookie value, or returns null.
 *
 * Every failure returns the same null: wrong key, tampered bytes, wrong purpose,
 * truncated input, unparseable JSON, expired. The caller's only correct response to
 * any of them is to treat the request as unauthenticated, and distinguishing them
 * would hand an attacker a decryption oracle.
 */
export function unseal<T>(
  purpose: Purpose,
  token: string,
  key: Buffer,
  now: number,
): Sealed<T> | null {
  if (key.length !== 32 || !token) return null

  let raw: Buffer
  try {
    raw = Buffer.from(token, 'base64url')
  } catch {
    return null
  }
  if (raw.length <= 1 + IV_BYTES + TAG_BYTES) return null
  if (raw[0] !== VERSION) return null

  const iv = raw.subarray(1, 1 + IV_BYTES)
  const tag = raw.subarray(1 + IV_BYTES, 1 + IV_BYTES + TAG_BYTES)
  const body = raw.subarray(1 + IV_BYTES + TAG_BYTES)

  let plaintext: Buffer
  try {
    const decipher = createDecipheriv('aes-256-gcm', key, iv)
    decipher.setAAD(Buffer.from(purpose, 'utf8'))
    decipher.setAuthTag(tag)
    plaintext = Buffer.concat([decipher.update(body), decipher.final()])
  } catch {
    return null
  }

  let parsed: unknown
  try {
    parsed = JSON.parse(plaintext.toString('utf8'))
  } catch {
    return null
  }
  if (typeof parsed !== 'object' || parsed === null) return null

  const record = parsed as { v?: unknown; e?: unknown }
  if (typeof record.e !== 'number' || !Number.isFinite(record.e)) return null
  if (record.e <= now) return null

  return { value: record.v as T, expiresAt: record.e }
}

/** open returns just the payload. The common case: the caller wants the credential. */
export function open<T>(purpose: Purpose, token: string, key: Buffer, now: number): T | null {
  return unseal<T>(purpose, token, key, now)?.value ?? null
}

/**
 * expiryOf returns the sealed expiry without the payload, so a countdown can be
 * rendered by code that never has the credential in scope. Null on any failure.
 */
export function expiryOf(purpose: Purpose, token: string, key: Buffer, now: number): number | null {
  return unseal<unknown>(purpose, token, key, now)?.expiresAt ?? null
}

/**
 * sameSecret compares two credentials without leaking which byte differed. Used where
 * the console needs to know whether a pasted key is the one it already holds — never
 * to authenticate anything, which is both services' job.
 */
export function sameSecret(a: string, b: string): boolean {
  const left = Buffer.from(a, 'utf8')
  const right = Buffer.from(b, 'utf8')
  if (left.length !== right.length) return false
  return timingSafeEqual(left, right)
}
