// The only place in this app that makes an outbound request.
//
// Everything above it — server components, route handlers — asks for a path and gets
// parsed data or an ApiError. Which credential a call is made with is decided here, from
// the call's declared authority, and never passed in by a caller. That is the whole point
// of the module: a page cannot accidentally spend operator authority on a read, and no
// path exists by which a credential reaches a response body.

import { randomUUID } from 'node:crypto'
import {
  ApiError,
  ERR_MALFORMED,
  ERR_TIMEOUT,
  ERR_UNREACHABLE,
  type Parsed,
  parseEnvelope,
} from './envelope'
import { config } from './env'
import { readOperatorKey, readSession } from './session'

/**
 * Authority declares what a goal-engine call needs.
 *
 * `bot` — the console's own key from the environment. Every read, and engaging the kill
 * switch, which the engine deliberately does not route-gate.
 *
 * `operator` — a key the operator pasted, held sealed in their cookie. Only two calls in
 * the whole engine need it (PATCH /v1/goals/:id and POST /v1/approvals/:id/resolve), plus
 * releasing the kill switch, which the engine enforces in the service rather than on the
 * route.
 *
 * `preferOperator` — send the operator key when one is held, otherwise the bot key. This
 * exists for exactly one route: PUT /v1/flags/kill-switch, where engaging is open to
 * anyone and releasing is an operator's. Sending the bot key when nobody holds authority
 * is correct there — the engine will accept an engage and refuse a release, which is the
 * behaviour the safety model wants. Do not widen this to other routes.
 */
export type Authority = 'bot' | 'operator' | 'preferOperator'

export type Call = {
  method?: 'GET' | 'POST' | 'PUT' | 'PATCH' | 'DELETE'
  /** Sent as JSON. Absent for reads. */
  body?: unknown
  /** Forwarded so one browser action is traceable through both services. */
  requestId?: string
  /** Overrides the sealed cookie. Used only by sign-in, which has no cookie yet. */
  token?: string
  /** Allows a call with no credential at all. Only the two open auth routes. */
  anonymous?: boolean
  /**
   * The browser's own User-Agent, forwarded on sign-in only.
   *
   * Core records it against the session and shows it back in "your devices", where the
   * whole point is recognising an unfamiliar sign-in. Without this every web session would
   * be listed as the console's HTTP client, which is the same string for everybody.
   *
   * Not a general header escape hatch, and deliberately not accompanied by one for
   * X-Forwarded-For: whether an address may be claimed by a proxy is core's own trust
   * setting, not something this console gets to assert.
   */
  userAgent?: string
}

/** newRequestId returns an id both services will accept: ASCII, well under 64 bytes. */
export function newRequestId(): string {
  return `web-${randomUUID()}`
}

/**
 * callEngine talks to the goal engine.
 *
 * The credential is chosen from `authority` and nothing else. A call declared `operator`
 * with no operator key held is refused here rather than sent with the bot key: the engine
 * would answer 403 anyway, and refusing locally means the message says which authority is
 * missing instead of reading as an outage.
 */
export async function callEngine<T>(
  path: string,
  authority: Authority,
  call: Call = {},
): Promise<Parsed<T>> {
  const cfg = config()
  const operatorKey = authority === 'bot' ? null : await readOperatorKey()

  if (authority === 'operator' && !operatorKey) {
    throw new ApiError({
      status: 403,
      code: 'FORBIDDEN',
      message: 'That needs operator authority. Paste your operator key to hold it.',
    })
  }

  const secret = operatorKey ?? cfg.goalEngineBotKey
  return send<T>(cfg.goalEngineBaseURL, path, call, secret)
}

/**
 * callCore talks to core as the signed-in person.
 *
 * There is no machine key for core in this app's environment, on purpose. Everything the
 * console reads out of core is somebody's own — their chats, their runs, their ledger,
 * their sessions, their linked channels — and a machine key here would be a way to read
 * all of it at once. If nobody is signed in, there is nothing to ask for.
 */
export async function callCore<T>(path: string, call: Call = {}): Promise<Parsed<T>> {
  const cfg = config()
  if (call.anonymous) return send<T>(cfg.coreBaseURL, path, call, null)

  const token = call.token ?? (await readSession())?.token ?? null
  if (!token) {
    throw new ApiError({
      status: 401,
      code: 'UNAUTHORIZED',
      message: 'Sign in to read that.',
    })
  }
  return send<T>(cfg.coreBaseURL, path, call, token)
}

/**
 * What a pasted goal-engine key turned out to be.
 *
 * `operator` — accepted, and the engine treats it as an operator.
 * `insufficient` — a real key, but a bot key. Holding it would buy nothing.
 * `unknown` — the engine does not recognise it.
 */
export type KeyKind = 'operator' | 'insufficient' | 'unknown'

/**
 * verifyOperatorKey checks a pasted key before it is sealed into a cookie.
 *
 * The probe is `POST /v1/approvals/<a fresh uuid>/resolve`, and it is chosen because it is
 * the only side-effect-free way to ask "is this key an operator's?". In the engine's
 * service, resolving runs requireOperator and then validates the resolution before it ever
 * looks the approval up, and the audit row is written only after a successful resolve. So a
 * random id gives:
 *
 *   404  the key is an operator's, and there was no such approval to resolve
 *   403  the key is valid but is a bot key
 *   401  the engine does not know the key
 *
 * Nothing is created, nothing is resolved, nothing is audited. Verifying at paste time
 * matters because the alternative is discovering the key was wrong at the moment somebody
 * is trying to answer a spend request.
 *
 * This is the one function that takes a credential as an argument. It exists so that the
 * generic call path never does: a caller cannot ask send() to use a key of its choosing.
 */
export async function verifyOperatorKey(key: string): Promise<KeyKind> {
  const cfg = config()
  const path = `/v1/approvals/${randomUUID()}/resolve`
  try {
    await send<unknown>(
      cfg.goalEngineBaseURL,
      path,
      { method: 'POST', body: { resolution: 'approved' } },
      key,
    )
  } catch (error) {
    if (error instanceof ApiError) {
      if (error.status === 404) return 'operator'
      if (error.status === 403) return 'insufficient'
      if (error.status === 401) return 'unknown'
    }
    throw error
  }
  // A 2xx would mean the uuid named a real open approval and it has just been resolved,
  // which cannot happen from a freshly generated id. Treat it as not a verification.
  throw new ApiError({
    status: 500,
    code: 'INTERNAL_ERROR',
    message: 'The engine answered a key check in a way the console cannot interpret.',
  })
}

/** send performs the request and turns every outcome into data or an ApiError. */
async function send<T>(
  baseURL: string,
  path: string,
  call: Call,
  secret: string | null,
): Promise<Parsed<T>> {
  const cfg = config()
  const method = call.method ?? 'GET'
  const requestId = call.requestId || newRequestId()

  const headers: Record<string, string> = {
    Accept: 'application/json',
    'X-Request-Id': requestId,
  }
  if (secret) headers.Authorization = `Bearer ${secret}`
  if (call.body !== undefined) headers['Content-Type'] = 'application/json'
  if (call.userAgent) headers['User-Agent'] = call.userAgent

  let response: Response
  try {
    response = await fetch(`${baseURL}${path}`, {
      method,
      headers,
      body: call.body === undefined ? undefined : JSON.stringify(call.body),
      // Nothing this console reads is cacheable: the whole screen is "what is true right
      // now", and a cached approval queue is a queue somebody acts on twice.
      cache: 'no-store',
      signal: AbortSignal.timeout(cfg.upstreamTimeoutMs),
    })
  } catch (error) {
    // The URL is deliberately not in the message. It carries a private address, and on a
    // timeout the operator's question is which service, not which host.
    const timedOut = error instanceof Error && error.name === 'TimeoutError'
    throw new ApiError({
      status: 0,
      code: timedOut ? ERR_TIMEOUT : ERR_UNREACHABLE,
      message: timedOut
        ? `The service did not answer within ${cfg.upstreamTimeoutMs}ms.`
        : 'The service could not be reached.',
      requestId,
    })
  }

  const text = await response.text()
  let body: unknown = null
  if (text !== '') {
    try {
      body = JSON.parse(text)
    } catch {
      throw new ApiError({
        status: response.status,
        code: ERR_MALFORMED,
        message: 'The service answered with something that was not JSON.',
        requestId,
      })
    }
  }

  return parseEnvelope<T>(response.status, body)
}
