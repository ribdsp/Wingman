import { describe, expect, test } from 'vitest'
import { PROXY_ROUTES, matchProxyRoute, paramAccepted, upstreamPath } from './proxy-routes'

function segs(path: string): string[] {
  return path.split('/').filter((part) => part !== '')
}

describe('matchProxyRoute', () => {
  test('a listed read matches and declares the bot key', () => {
    const rule = matchProxyRoute('engine', 'GET', segs('/v1/evaluations/latest'))

    expect(rule?.authority).toBe('bot')
  })

  test('a parameter segment matches one segment', () => {
    const rule = matchProxyRoute('engine', 'GET', segs('/v1/goals/0f8c-1234/evaluations'))

    expect(rule?.pattern).toBe('/v1/goals/:id/evaluations')
  })

  test('resolving an approval requires operator authority, not the bot key', () => {
    const rule = matchProxyRoute('engine', 'POST', segs('/v1/approvals/abc/resolve'))

    expect(rule?.authority).toBe('operator')
  })

  test('moving a target requires operator authority', () => {
    // An agent that could lower the bar it is measured against would be marking its own
    // homework; the engine gates this on the route, and so does the console.
    const rule = matchProxyRoute('engine', 'PATCH', segs('/v1/goals/abc'))

    expect(rule?.authority).toBe('operator')
  })

  test('the kill switch prefers the operator key and falls back to the bot key', () => {
    // Engaging must work with no authority held. Releasing must not, and the engine is
    // what refuses it — the console does not need a second, weaker rule here.
    const rule = matchProxyRoute('engine', 'PUT', segs('/v1/flags/kill-switch'))

    expect(rule?.authority).toBe('preferOperator')
  })

  test.each([
    ['a metric sample, which would falsify a goal', 'POST', '/v1/metrics/ops.tokens_spent/samples'],
    ['a monitor tick, which would start agent work', 'POST', '/v1/monitor/tick'],
    ['creating a goal', 'POST', '/v1/goals'],
    ['filing a spend request', 'POST', '/v1/approvals'],
    ['expiring approvals', 'POST', '/v1/approvals/expire'],
  ])('the engine will not proxy %s', (_name, method, path) => {
    expect(matchProxyRoute('engine', method, segs(path))).toBeNull()
  })

  test.each([
    ['dispatching unattended work', 'POST', '/v1/tasks'],
    ['the goal-engine dispatch path', 'POST', '/api/v1/tasks'],
    ['creating an account', 'POST', '/v1/accounts'],
    ['listing every account', 'GET', '/v1/accounts'],
    ['deactivating an account', 'PUT', '/v1/accounts/abc/active'],
    ['open registration', 'POST', '/v1/auth/register'],
    // Both of these end the session the console is holding, so they belong to a handler
    // that can also clear the cookie.
    ['changing a password', 'PUT', '/v1/me/password'],
    ['signing out everywhere', 'DELETE', '/v1/me/sessions'],
    ['signing out', 'POST', '/v1/auth/signout'],
  ])('core will not proxy %s', (_name, method, path) => {
    expect(matchProxyRoute('core', method, segs(path))).toBeNull()
  })

  test('a core route is never matched against the engine, or the reverse', () => {
    expect(matchProxyRoute('engine', 'GET', segs('/v1/me/ledger'))).toBeNull()
    expect(matchProxyRoute('core', 'GET', segs('/v1/evaluations/latest'))).toBeNull()
  })

  test('the right path with the wrong method does not match', () => {
    expect(matchProxyRoute('engine', 'DELETE', segs('/v1/goals/abc'))).toBeNull()
    expect(matchProxyRoute('engine', 'POST', segs('/v1/flags/kill-switch'))).toBeNull()
  })

  test('a longer or shorter path does not match a pattern', () => {
    expect(matchProxyRoute('engine', 'GET', segs('/v1/goals/abc/evaluations/extra'))).toBeNull()
    expect(matchProxyRoute('engine', 'GET', segs('/v1/goals/abc/evaluations/'))).not.toBeNull()
    expect(matchProxyRoute('engine', 'POST', segs('/v1/approvals/resolve'))).toBeNull()
  })

  test.each([
    ['a traversal segment', ['v1', 'goals', '..', 'metrics']],
    ['a dot segment', ['v1', 'goals', '.']],
    ['a decoded slash', ['v1', 'goals', 'a/b']],
    ['a query string smuggled into a segment', ['v1', 'goals', 'abc?x=1']],
    ['an empty segment', ['v1', 'goals', '']],
    ['a segment past the length bound', ['v1', 'goals', 'a'.repeat(129)]],
    ['nothing at all', []],
  ])('%s is refused', (_name, segments) => {
    expect(matchProxyRoute('engine', 'GET', segments)).toBeNull()
  })

  test('every core rule declares no authority, because core is called as the person', () => {
    // A core rule that named an authority would be a rule someone could satisfy with a
    // machine key. This app holds no core machine key at all.
    for (const rule of PROXY_ROUTES.filter((r) => r.service === 'core')) {
      expect(rule.authority).toBeUndefined()
    }
  })

  test('every engine rule declares an authority explicitly', () => {
    for (const rule of PROXY_ROUTES.filter((r) => r.service === 'engine')) {
      expect(rule.authority).toBeDefined()
    }
  })

  test('only the three known write operations may reach the engine', () => {
    const writes = PROXY_ROUTES.filter(
      (rule) => rule.service === 'engine' && rule.method !== 'GET',
    ).map((rule) => `${rule.method} ${rule.pattern}`)

    expect(writes.sort()).toEqual([
      'PATCH /v1/goals/:id',
      'POST /v1/approvals/:id/resolve',
      'PUT /v1/flags/kill-switch',
    ])
  })
})

describe('upstreamPath', () => {
  test('a recognised parameter survives', () => {
    expect(upstreamPath(segs('/v1/approvals'), '?openOnly=true&limit=50')).toBe(
      '/v1/approvals?openOnly=true&limit=50',
    )
  })

  test('an unrecognised parameter is dropped', () => {
    expect(upstreamPath(segs('/v1/audit'), '?actorType=agent&secret=x')).toBe(
      '/v1/audit?actorType=agent',
    )
  })

  test('a recognised name with an unrecognised value is dropped', () => {
    expect(upstreamPath(segs('/v1/approvals'), '?openOnly=maybe')).toBe('/v1/approvals')
  })

  test('an rfc3339 window survives, colons and offset included', () => {
    expect(upstreamPath(segs('/v1/audit'), '?since=2026-09-13T00:00:00%2B07:00')).toBe(
      '/v1/audit?since=2026-09-13T00%3A00%3A00%2B07%3A00',
    )
  })

  test('no query string yields a bare path', () => {
    expect(upstreamPath(segs('/v1/goals'), '')).toBe('/v1/goals')
  })
})

describe('paramAccepted', () => {
  // The audit filter asks this before putting a value in the URL, so that a box holding a
  // value the proxy would silently drop says so instead of appearing to filter.
  test.each([
    ['actorType', 'agent'],
    ['action', 'goal.target.updated'],
    ['subjectId', 'a1B2-c3_d4.e5'],
    ['outcome', 'denied'],
    ['since', '2026-09-13T00:00:00+07:00'],
    ['limit', '60'],
    ['offset', '120'],
    ['openOnly', 'false'],
  ])('%s accepts %s', (name, value) => {
    expect(paramAccepted(name, value)).toBe(true)
  })

  test.each([
    ['an uppercase actor type the engine does not use', 'actorType', 'Agent'],
    ['a wildcard in an action', 'action', 'goal.*'],
    ['a boolean spelled as a word', 'openOnly', 'yes'],
    ['a limit past four digits', 'limit', '100000'],
    ['a negative offset', 'offset', '-1'],
    ['a timestamp with a space in it', 'since', '2026-09-13 00:00:00'],
    ['a subject id carrying a path', 'subjectId', '../secrets'],
    ['a search with a quote in it', 'search', "o'brien"],
  ])('%s is refused', (_name, name, value) => {
    expect(paramAccepted(name, value)).toBe(false)
  })

  test('a name not on the table is refused whatever its value', () => {
    expect(paramAccepted('userId', 'abc')).toBe(false)
    expect(paramAccepted('order', 'asc')).toBe(false)
  })

  test('an empty value is refused rather than forwarded as a filter on nothing', () => {
    // Every pattern requires at least one character, so clearing a box drops the parameter.
    expect(paramAccepted('actorType', '')).toBe(false)
    expect(paramAccepted('limit', '')).toBe(false)
  })
})
