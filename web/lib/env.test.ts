import { describe, expect, test } from 'vitest'
import { load } from './env'

const secret = Buffer.alloc(32, 3).toString('base64')

function valid(overrides: Record<string, string | undefined> = {}) {
  return {
    GOAL_ENGINE_BASE_URL: 'http://127.0.0.1:8080',
    CORE_BASE_URL: 'http://127.0.0.1:8081',
    GOAL_ENGINE_BOT_KEY: 'web:bot-secret',
    WEB_COOKIE_SECRET: secret,
    ...overrides,
  }
}

describe('load', () => {
  test('a complete environment yields the documented defaults', () => {
    const cfg = load(valid())

    expect(cfg.operatorTtlMinutes).toBe(30)
    expect(cfg.pollIntervalMs).toBe(4000)
    expect(cfg.upstreamTimeoutMs).toBe(15_000)
    expect(cfg.cookieKey).toHaveLength(32)
  })

  test('a trailing slash is stripped so joined paths are not double-slashed', () => {
    const cfg = load(valid({ CORE_BASE_URL: 'http://127.0.0.1:8081///' }))

    expect(cfg.coreBaseURL).toBe('http://127.0.0.1:8081')
  })

  test('CHANGE_ME is treated as unset, not as a credential', () => {
    // The .env.example ships CHANGE_ME. Accepting it would mean a console that boots
    // and then authenticates as nobody, which looks like a service outage.
    expect(() => load(valid({ GOAL_ENGINE_BOT_KEY: 'CHANGE_ME' }))).toThrow(
      /GOAL_ENGINE_BOT_KEY is required/,
    )
  })

  test('every problem is reported at once rather than one restart at a time', () => {
    let message = ''
    try {
      load({})
    } catch (error) {
      message = error instanceof Error ? error.message : String(error)
    }

    expect(message).toContain('GOAL_ENGINE_BASE_URL')
    expect(message).toContain('CORE_BASE_URL')
    expect(message).toContain('GOAL_ENGINE_BOT_KEY')
    expect(message).toContain('WEB_COOKIE_SECRET')
  })

  test('the failure message names variables and never their values', () => {
    let message = ''
    try {
      load(valid({ WEB_COOKIE_SECRET: 'too-short' }))
    } catch (error) {
      message = error instanceof Error ? error.message : String(error)
    }

    expect(message).toContain('WEB_COOKIE_SECRET must decode to 32 bytes')
    expect(message).not.toContain('too-short')
  })

  test('a hex cookie secret is accepted as well as base64', () => {
    const cfg = load(valid({ WEB_COOKIE_SECRET: 'a'.repeat(64) }))

    expect(cfg.cookieKey).toHaveLength(32)
  })

  test('a non-http URL is refused', () => {
    expect(() => load(valid({ CORE_BASE_URL: 'ftp://example.test' }))).toThrow(
      /CORE_BASE_URL must be an http or https URL/,
    )
  })

  test.each([
    ['WEB_OPERATOR_TTL_MINUTES', '0', /at least 1/],
    ['WEB_OPERATOR_TTL_MINUTES', '481', /at most 480/],
    ['WEB_POLL_INTERVAL_MS', '999', /at least 1000/],
    ['WEB_UPSTREAM_TIMEOUT_MS', '0', /at least 1000/],
  ])('%s=%s is refused rather than clamped silently', (name, value, expected) => {
    // Silently clamping would mean an operator who set 0 believing it meant "no
    // limit" never finds out it meant something else.
    expect(() => load(valid({ [name]: value }))).toThrow(expected)
  })

  test('a non-numeric bound is refused', () => {
    expect(() => load(valid({ WEB_POLL_INTERVAL_MS: 'fast' }))).toThrow(/must be a number/)
  })

  test('an empty bound falls back to its default', () => {
    const cfg = load(valid({ WEB_POLL_INTERVAL_MS: '' }))

    expect(cfg.pollIntervalMs).toBe(4000)
  })
})
