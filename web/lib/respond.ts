// This app's own JSON responses.
//
// Same envelope as the two Go services — success, code, message, data, error, meta — because
// a client component should not have to know whether an answer came from the console or was
// forwarded from a service. Inventing a second shape here would mean two parsers in the
// browser, and the second one would be the one with the bug.
//
// The one field that is not copied through is `error.message` on a failure the console
// generated itself: those are written for the person reading the screen, since they are
// about held authority and reachability rather than about a request being wrong.

import { ApiError, ERR_TIMEOUT, ERR_UNREACHABLE, type Pagination, type Parsed } from './envelope'

type Meta = {
  requestId: string
  timestamp: string
  pagination?: Pagination
}

type Body = {
  success: boolean
  code: number
  message: string
  data?: unknown
  error?: { code: string; message: string; fields?: Record<string, string> }
  meta: Meta
}

/** A status the browser can act on, for the two failures that have no upstream status. */
function statusOf(error: ApiError): number {
  if (error.status >= 400) return error.status
  if (error.code === ERR_TIMEOUT) return 504
  if (error.code === ERR_UNREACHABLE) return 502
  return 502
}

function send(body: Body, status: number): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: {
      'Content-Type': 'application/json; charset=utf-8',
      // Echoed so one action is traceable from the browser's network panel through both
      // services' access logs.
      'X-Request-Id': body.meta.requestId,
      'Cache-Control': 'no-store',
    },
  })
}

/** ok answers with data the console produced itself. */
export function ok(data: unknown, requestId: string, status = 200): Response {
  return send(
    {
      success: true,
      code: status,
      message: 'ok',
      data,
      meta: { requestId, timestamp: new Date().toISOString() },
    },
    status,
  )
}

/**
 * forward re-serialises a parsed upstream answer.
 *
 * The service's own status is kept — 201 for a created task, 200 for a read — because the
 * console guessing one would mean a client seeing 201 for a cancel that answered 200. The
 * upstream requestId is kept too: it is the id both services logged the call under, so it
 * is the one worth being able to search for.
 */
export function forward<T>(parsed: Parsed<T>): Response {
  const meta: Meta = { requestId: parsed.requestId, timestamp: parsed.timestamp }
  if (parsed.pagination) meta.pagination = parsed.pagination
  return send(
    {
      success: true,
      code: parsed.status,
      message: parsed.message,
      data: parsed.data,
      meta,
    },
    parsed.status,
  )
}

/**
 * fail answers with an error, whatever went wrong.
 *
 * An unknown throw becomes a plain 500 with no detail: this app runs beside two API keys
 * and a cookie key, and a stack or a driver string in a response body is how those leave a
 * process. The real cause is on the server's stderr.
 */
export function fail(error: unknown, requestId: string): Response {
  if (error instanceof ApiError) {
    const status = statusOf(error)
    const body: Body = {
      success: false,
      code: status,
      message: error.message,
      error: { code: error.code, message: error.message },
      meta: { requestId: error.requestId || requestId, timestamp: new Date().toISOString() },
    }
    if (Object.keys(error.fields).length > 0) body.error!.fields = error.fields
    return send(body, status)
  }

  return send(
    {
      success: false,
      code: 500,
      message: 'The console failed to handle that.',
      error: { code: 'INTERNAL_ERROR', message: 'The console failed to handle that.' },
      meta: { requestId, timestamp: new Date().toISOString() },
    },
    500,
  )
}

/** notFound is the answer for a path the proxy table does not list. */
export function notFound(requestId: string): Response {
  return send(
    {
      success: false,
      code: 404,
      message: 'No such route.',
      error: { code: 'NOT_FOUND', message: 'No such route.' },
      meta: { requestId, timestamp: new Date().toISOString() },
    },
    404,
  )
}
