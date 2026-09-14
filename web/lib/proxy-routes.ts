// What a browser is allowed to ask this server to forward.
//
// The two route handlers under app/api are proxies, and a proxy that forwards whatever it
// is given is not a proxy but a credential loan. The console holds a goal-engine key and,
// sometimes, an operator key; a pass-through would let anything running in the page reach
// POST /v1/metrics/:key/samples and falsify the number a goal is measured against, or
// POST /api/v1/tasks on core and start unattended work.
//
// So: an explicit table. Method plus path pattern, and for the engine the authority the
// call is made with. A request that does not match a row is refused here, before any
// credential is chosen — and adding a row is a decision, which is the point.
//
// Reads that a server component can do directly are still listed when a client component
// polls them, because polling is what the deck does.

import type { Authority } from './upstream'

export type Method = 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
export type Service = 'engine' | 'core'

export type ProxyRule = {
  method: Method
  /** Upstream path. `:name` matches exactly one segment. */
  pattern: string
  service: Service
  /** Goal engine only. Core is always called as the signed-in person. */
  authority?: Authority
}

/**
 * ENGINE_ROUTES is every goal-engine call the console can make.
 *
 * Absent on purpose, and each for a reason:
 *
 *   POST /v1/goals, POST /v1/approvals   the console is a place to watch and decide, not
 *                                        a place to author goals or file spend requests;
 *                                        the engine's own clients do that
 *   POST /v1/metrics/:key/samples        a writable metric reachable from a page is a
 *                                        falsifiable target
 *   POST /v1/monitor/tick                a tick is the worker's job; a button that ran one
 *                                        would let a refresh loop trigger agent work
 *   POST /v1/approvals/expire            same: a maintenance sweep, not an interaction
 */
const ENGINE_ROUTES: readonly ProxyRule[] = [
  { method: 'GET', pattern: '/v1/goals', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/goals/:id', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/goals/:id/evaluations', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/evaluations/latest', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/approvals', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/approvals/:id', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/flags', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/flags/kill-switch', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/audit', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/metrics', service: 'engine', authority: 'bot' },
  { method: 'GET', pattern: '/v1/metrics/:key', service: 'engine', authority: 'bot' },
  {
    method: 'GET',
    pattern: '/v1/metrics/:key/samples/latest',
    service: 'engine',
    authority: 'bot',
  },

  // The one button that changes the world without holding authority first. Engaging is
  // open to anyone the engine will talk to — deliberately, because whoever notices the
  // damage should be able to stop it — and releasing is refused to a bot key inside the
  // engine's service. preferOperator sends whichever is held, and the engine decides.
  { method: 'PUT', pattern: '/v1/flags/kill-switch', service: 'engine', authority: 'preferOperator' },

  // The two calls the engine gates on the route. Refused locally when no operator key is
  // held, so the message names the missing authority instead of looking like an outage.
  {
    method: 'POST',
    pattern: '/v1/approvals/:id/resolve',
    service: 'engine',
    authority: 'operator',
  },
  { method: 'PATCH', pattern: '/v1/goals/:id', service: 'engine', authority: 'operator' },
]

/**
 * CORE_ROUTES is every core call the console can make, all of them as the signed-in
 * person's own session.
 *
 * Absent on purpose:
 *
 *   POST /v1/tasks, POST /api/v1/tasks   dispatching unattended work is the goal engine's
 *                                        job. Core refuses it to a person anyway; not
 *                                        having the row means the console never asks
 *   POST /v1/accounts, GET /v1/accounts, PUT /v1/accounts/:id/active
 *                                        account administration is operator-only in core,
 *                                        and this console holds no core machine key at
 *                                        all — see callCore for why
 *   POST /v1/auth/register               registration is closed by default and the first
 *                                        account is made by `core createuser`
 *   PUT /v1/me/password, DELETE /v1/me/sessions
 *                                        both end every session the caller has, including
 *                                        the one this console is holding. Proxying them
 *                                        would leave the cookie alive and every subsequent
 *                                        read answering 401. They belong to a handler that
 *                                        also clears the cookie, which is not this one
 *   POST /v1/auth/signout                same reason, and it is /api/session DELETE's job
 *   POST /v1/notifications               the goal engine's outbound edge, not a button. Core
 *                                        refuses it to a person, the recipient is fixed to
 *                                        the unattended owner rather than chosen by a
 *                                        caller, and a page that could send one could send
 *                                        a message about a decision that never happened
 */
const CORE_ROUTES: readonly ProxyRule[] = [
  { method: 'GET', pattern: '/v1/me', service: 'core' },
  { method: 'GET', pattern: '/v1/me/ledger', service: 'core' },
  { method: 'GET', pattern: '/v1/me/sessions', service: 'core' },
  { method: 'DELETE', pattern: '/v1/me/sessions/:id', service: 'core' },

  { method: 'GET', pattern: '/v1/chats', service: 'core' },
  { method: 'POST', pattern: '/v1/chats', service: 'core' },
  { method: 'GET', pattern: '/v1/chats/:id', service: 'core' },
  { method: 'GET', pattern: '/v1/chats/:id/messages', service: 'core' },
  { method: 'PUT', pattern: '/v1/chats/:id/title', service: 'core' },
  { method: 'POST', pattern: '/v1/chats/:id/archive', service: 'core' },
  { method: 'POST', pattern: '/v1/messages', service: 'core' },

  { method: 'GET', pattern: '/v1/tasks', service: 'core' },
  { method: 'GET', pattern: '/v1/tasks/:id', service: 'core' },
  { method: 'GET', pattern: '/v1/tasks/:id/runs', service: 'core' },
  { method: 'GET', pattern: '/v1/runs/:id', service: 'core' },
  { method: 'GET', pattern: '/v1/runs/:id/steps', service: 'core' },
  { method: 'GET', pattern: '/v1/runs/:id/cost', service: 'core' },
  { method: 'POST', pattern: '/v1/runs/:id/cancel', service: 'core' },

  { method: 'GET', pattern: '/v1/channels', service: 'core' },
  { method: 'POST', pattern: '/v1/channels/link-codes', service: 'core' },
  { method: 'DELETE', pattern: '/v1/channels/:id', service: 'core' },
]

export const PROXY_ROUTES: readonly ProxyRule[] = [...ENGINE_ROUTES, ...CORE_ROUTES]

/**
 * A path segment standing in for `:name`. Ids are uuids and metric keys look like
 * `ops.tokens_spent`, so letters, digits, dash, underscore and dot are enough.
 *
 * Anything else is refused rather than forwarded. A segment holding an encoded slash, a
 * query string or `..` is an attempt to reach a path this table does not list, which is
 * the one thing the table exists to prevent.
 */
const SAFE_SEGMENT = /^[A-Za-z0-9._-]{1,128}$/

/**
 * matchProxyRoute returns the rule for a request, or null.
 *
 * `segments` is the decoded path as Next hands it over, already split. Taking it split
 * rather than as a string means there is no place for an encoded separator to be decoded
 * into one after the check.
 */
export function matchProxyRoute(
  service: Service,
  method: string,
  segments: readonly string[],
): ProxyRule | null {
  if (segments.length === 0) return null
  for (const segment of segments) {
    if (!SAFE_SEGMENT.test(segment)) return null
    if (segment === '.' || segment === '..') return null
  }

  for (const rule of PROXY_ROUTES) {
    if (rule.service !== service) continue
    if (rule.method !== method) continue
    if (matches(rule.pattern, segments)) return rule
  }
  // Nothing matched. The caller answers 404 rather than 403 — this table is the console's
  // own, and its shape is not something a caller needs to learn by probing.
  return null
}

function matches(pattern: string, segments: readonly string[]): boolean {
  const expected = pattern.split('/').filter((part) => part !== '')
  if (expected.length !== segments.length) return false
  for (let i = 0; i < expected.length; i += 1) {
    const part = expected[i]
    if (part === undefined) return false
    if (part.startsWith(':')) continue
    if (part !== segments[i]) return false
  }
  return true
}

/**
 * QUERY_PARAMS is every query parameter the two services read, with a pattern for each.
 *
 * Taken from their handlers, not invented: page/limit/offset are shared, openOnly and
 * actionType/outcome are the approval queue's, actorType/action/subjectType/subjectId and
 * the since/until window are the audit log's, product/status/metricKey/search are the goal
 * list's, includeArchived is core's chat list. A name not on this list is dropped rather
 * than forwarded — a proxy that passes an unknown parameter through is a proxy that will
 * one day pass through a parameter that means something.
 */
const QUERY_PARAMS: Readonly<Record<string, RegExp>> = {
  page: /^[0-9]{1,6}$/,
  limit: /^[0-9]{1,4}$/,
  offset: /^[0-9]{1,9}$/,
  openOnly: /^(true|false)$/,
  includeArchived: /^(true|false)$/,
  actionType: /^[a-z0-9_.-]{1,64}$/,
  outcome: /^[a-z_]{1,32}$/,
  actorType: /^[a-z_]{1,32}$/,
  action: /^[a-z0-9_.-]{1,64}$/,
  subjectType: /^[a-z0-9_.-]{1,64}$/,
  subjectId: /^[A-Za-z0-9._-]{1,128}$/,
  product: /^[A-Za-z0-9 ._-]{1,64}$/,
  status: /^[a-z_]{1,32}$/,
  metricKey: /^[a-z0-9_.-]{1,128}$/,
  // Free text, matched against goal names and products. Restricted to the same charset
  // as product rather than accepting anything printable: a value from here reaches a log
  // line, and a search box is not a reason to widen what may appear in one.
  search: /^[A-Za-z0-9 ._-]{1,128}$/,
  // RFC 3339, which the audit filter parses with time.Parse and rejects otherwise.
  since: /^[0-9T:+.Z-]{1,40}$/,
  until: /^[0-9T:+.Z-]{1,40}$/,
}

/**
 * paramAccepted reports whether a value would survive upstreamPath.
 *
 * Exported so the audit filter can refuse a value locally instead of pushing one the server
 * then drops: a filter box holding a value that visibly has no effect is worse than one that
 * says the log does not accept it. One table, both answers.
 */
export function paramAccepted(name: string, value: string): boolean {
  const pattern = QUERY_PARAMS[name]
  return pattern !== undefined && pattern.test(value)
}

/**
 * upstreamPath rebuilds the path to forward, keeping only recognised parameters whose
 * values look like what the service expects. Only ever called with matched segments.
 */
export function upstreamPath(segments: readonly string[], search: string): string {
  const path = `/${segments.join('/')}`
  const allowed = new URLSearchParams()
  for (const [name, value] of new URLSearchParams(search)) {
    if (paramAccepted(name, value)) allowed.set(name, value)
  }
  const query = allowed.toString()
  return query === '' ? path : `${path}?${query}`
}
