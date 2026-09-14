import { describe, expect, test } from 'vitest'
import { expiryOf, open, sameSecret, seal, unseal } from './seal'

const key = Buffer.alloc(32, 7)
const otherKey = Buffer.alloc(32, 9)
const now = 1_757_700_000_000
const soon = now + 60_000

describe('seal', () => {
  test('a sealed payload comes back unchanged under the same key and purpose', () => {
    const token = seal('session', { token: 'wgm_abc', userId: 'u-1' }, soon, key)

    const opened = open<{ token: string; userId: string }>('session', token, key, now)

    expect(opened).toEqual({ token: 'wgm_abc', userId: 'u-1' })
  })

  test('the cookie value does not contain the credential in readable form', () => {
    const token = seal('session', { token: 'wgm_abc' }, soon, key)

    // The point of the whole file: a cookie read off disk is not a usable credential.
    expect(token).not.toContain('wgm_abc')
    expect(Buffer.from(token, 'base64url').toString('utf8')).not.toContain('wgm_abc')
  })

  test('a token sealed for one purpose does not open as the other', () => {
    // Otherwise an operator key lifted from its cookie could be replayed into the
    // session slot, where a different set of rules applies to it.
    const token = seal('operator', 'ge_operator_key', soon, key)

    expect(open<string>('session', token, key, now)).toBeNull()
  })

  test('a different key opens nothing', () => {
    const token = seal('session', { token: 'wgm_abc' }, soon, key)

    expect(open<{ token: string }>('session', token, otherKey, now)).toBeNull()
  })

  test('flipping a single ciphertext byte is rejected rather than decrypted', () => {
    const token = seal('session', { token: 'wgm_abc' }, soon, key)
    const raw = Buffer.from(token, 'base64url')
    const last = raw.length - 1
    raw[last] = (raw[last] ?? 0) ^ 0x01

    expect(open<{ token: string }>('session', raw.toString('base64url'), key, now)).toBeNull()
  })

  test('flipping a byte of the authentication tag is rejected', () => {
    const token = seal('session', { token: 'wgm_abc' }, soon, key)
    const raw = Buffer.from(token, 'base64url')
    raw[14] = (raw[14] ?? 0) ^ 0xff

    expect(open<{ token: string }>('session', raw.toString('base64url'), key, now)).toBeNull()
  })

  test('an expiry that has passed is refused even though the bytes are authentic', () => {
    // The cookie's Max-Age is advice to the client; this is the enforcement.
    const token = seal('operator', 'ge_operator_key', now - 1, key)

    expect(open<string>('operator', token, key, now)).toBeNull()
  })

  test('an expiry exactly at now is refused, not accepted on the boundary', () => {
    const token = seal('operator', 'ge_operator_key', now, key)

    expect(open<string>('operator', token, key, now)).toBeNull()
  })

  test.each([
    ['empty', ''],
    ['not base64url', '!!!!'],
    ['too short to hold iv and tag', Buffer.alloc(20, 1).toString('base64url')],
    ['a wrong version byte', Buffer.alloc(64, 2).toString('base64url')],
  ])('%s opens as null rather than throwing', (_name, token) => {
    expect(() => open<unknown>('session', token, key, now)).not.toThrow()
    expect(open<unknown>('session', token, key, now)).toBeNull()
  })

  test('a 31-byte key is refused at seal time rather than silently weakening', () => {
    expect(() => seal('session', 'x', soon, Buffer.alloc(31, 1))).toThrow(/32 bytes/)
  })

  test('two seals of the same payload differ, because the iv is random', () => {
    const a = seal('session', { token: 'wgm_abc' }, soon, key)
    const b = seal('session', { token: 'wgm_abc' }, soon, key)

    expect(a).not.toEqual(b)
  })

  test('unseal returns the expiry alongside the payload', () => {
    const token = seal('operator', 'ge_operator_key', soon, key)

    expect(unseal<string>('operator', token, key, now)).toEqual({
      value: 'ge_operator_key',
      expiresAt: soon,
    })
  })

  test('expiryOf reads the countdown without returning the credential', () => {
    const token = seal('operator', 'ge_operator_key', soon, key)

    expect(expiryOf('operator', token, key, now)).toBe(soon)
  })

  test('expiryOf on an unopenable token is null, not a rendered countdown', () => {
    const token = seal('operator', 'ge_operator_key', soon, key)

    expect(expiryOf('operator', token, otherKey, now)).toBeNull()
  })
})

describe('sameSecret', () => {
  test('identical secrets match', () => {
    expect(sameSecret('ge_key_one', 'ge_key_one')).toBe(true)
  })

  test('different secrets of equal length do not match', () => {
    expect(sameSecret('ge_key_one', 'ge_key_two')).toBe(false)
  })

  test('a prefix does not match the longer secret', () => {
    expect(sameSecret('ge_key', 'ge_key_one')).toBe(false)
  })
})
