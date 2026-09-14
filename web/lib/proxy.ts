// The proxy both app/api route handlers are.
//
// One implementation, two thin route files, because the rules that matter — is this path
// listed, is this a same-site call, is the body a sane size, which credential does the call
// take — must not be able to differ between the engine proxy and the core proxy.
//
// Everything a browser can reach this way is in proxy-routes.ts. Everything about which
// credential is spent is in upstream.ts. This module is the part in between: reading the
// request without trusting it.

import { ApiError } from './envelope'
import { type Service, matchProxyRoute, upstreamPath } from './proxy-routes'
import { fail, forward, notFound } from './respond'
import { callCore, callEngine, newRequestId } from './upstream'

/**
 * A body cap. A chat message is prose and every other write here is a short JSON object;
 * 64 KiB is far past both. The services impose their own real limits — this is only so a
 * page cannot make the console buffer something enormous on its way to them.
 *
 * Refused as VALIDATION_ERROR rather than a tenth error code for "too large": adding one
 * would be an API change, and the nine are shared with both services.
 */
export const MAX_BODY_BYTES = 64 * 1024

/**
 * A request id the services will accept, and that nothing else can be smuggled in.
 *
 * Both read X-Request-Id inbound and echo it into their access logs, so keeping the
 * browser's id makes one click traceable end to end. Which also means it reaches a log
 * line: the charset here is narrower than what the services allow, because a log line is
 * not a place to accept arbitrary printable input.
 */
const SAFE_REQUEST_ID = /^[A-Za-z0-9._:-]{1,64}$/

export function inboundRequestId(header: string | null): string {
  if (header && SAFE_REQUEST_ID.test(header)) return header
  return newRequestId()
}

/**
 * isSameSite guards the state-changing methods.
 *
 * Both cookies are SameSite=Strict, so a cross-site request arrives without them and could
 * not spend authority anyway. This is the second lock: browsers send Sec-Fetch-Site on
 * every request, and a value that is not same-origin on a POST is not a console action.
 * An absent header is allowed — that is `curl` against a local instance, which is a
 * documented way to drive both services and has to send a credential of its own regardless.
 */
export function isSameSite(header: string | null): boolean {
  return header === null || header === 'same-origin'
}

/**
 * readBody parses a JSON body, or throws.
 *
 * No schema check. The body is forwarded to a service that validates it properly and
 * answers with field-level errors the console renders as they are; a second, weaker
 * validation here would be a second thing to keep in step with two Go services.
 */
export async function readBody(request: Request): Promise<unknown> {
  const text = await request.text()
  if (text === '') return undefined
  if (Buffer.byteLength(text, 'utf8') > MAX_BODY_BYTES) {
    throw new ApiError({
      status: 400,
      code: 'VALIDATION_ERROR',
      message: 'That request body is too large for the console to forward.',
    })
  }
  try {
    return JSON.parse(text) as unknown
  } catch {
    throw new ApiError({
      status: 400,
      code: 'VALIDATION_ERROR',
      message: 'That request body was not JSON.',
    })
  }
}

/**
 * proxy forwards one browser request, or refuses it.
 *
 * The order is the point:
 *
 *   1. is the method state-changing and the call cross-site        → 403, nothing read
 *   2. is this method-and-path listed for this service             → 404, no credential chosen
 *   3. read the body under a cap
 *   4. hand to upstream.ts, which picks the credential from the rule's authority
 *
 * Step 2 before step 4 means an unlisted path never reaches the code that holds a key.
 */
export async function proxy(
  service: Service,
  request: Request,
  segments: readonly string[],
): Promise<Response> {
  const requestId = inboundRequestId(request.headers.get('x-request-id'))
  const method = request.method.toUpperCase()

  if (method !== 'GET' && !isSameSite(request.headers.get('sec-fetch-site'))) {
    return fail(
      new ApiError({
        status: 403,
        code: 'FORBIDDEN',
        message: 'That request did not come from this console.',
        requestId,
      }),
      requestId,
    )
  }

  const rule = matchProxyRoute(service, method, segments)
  if (!rule) return notFound(requestId)

  try {
    const body = method === 'GET' ? undefined : await readBody(request)
    const path = upstreamPath(segments, new URL(request.url).search)
    const call = { method: rule.method, requestId, ...(body === undefined ? {} : { body }) }

    const parsed =
      rule.service === 'engine'
        ? // The rule carries the authority. A missing one would be a table entry someone
          // added without deciding, so it is refused rather than defaulted to the bot key.
          await callEngine<unknown>(path, rule.authority ?? 'operator', call)
        : await callCore<unknown>(path, call)

    return forward(parsed)
  } catch (error) {
    return fail(error, requestId)
  }
}
