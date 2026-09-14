// Reading the two services' shared response envelope.
//
// Both Go services answer with the same shape — success, code, message, data, error,
// meta — and nine stable error codes. This module is the only place that knows it.
// Everything above works with plain values and an ApiError.
//
// Deliberately not a schema library. What has to be checked here is one envelope and a
// handful of number-or-null fields; the house rule about a dependency needing a reason
// that survives "could this be 20 lines?" applies to the console as much as to Go.
// What is worth being strict about is the small set of fields where a wrong reading is
// dangerous — those get named guards below, and tests.

/** Pagination as the services send it: nested under meta, absent on single reads. */
export type Pagination = {
  page: number
  limit: number
  totalItems: number
  totalPages: number
}

export type Parsed<T> = {
  data: T
  /** The status the service answered with, so a proxy can forward it rather than guess. */
  status: number
  message: string
  requestId: string
  timestamp: string
  pagination: Pagination | null
}

/**
 * ApiError is a call that did not produce data: a transport failure, a timeout, or an
 * envelope with success false.
 *
 * A refusal is not one of these. A denied spend request is a 201 with
 * `outcome: "denied"`, and a halted run is a run record with a stop reason — both are
 * answers, and turning them into exceptions would let a `catch` swallow exactly the
 * decisions this console exists to display.
 */
export class ApiError extends Error {
  /** HTTP status, or 0 when the request never got an answer. */
  readonly status: number
  /** One of the nine shared codes, or a local code for transport failures. */
  readonly code: string
  readonly requestId: string
  /** Per-field messages, present on VALIDATION_ERROR. */
  readonly fields: Record<string, string>

  constructor(args: {
    status: number
    code: string
    message: string
    requestId?: string
    fields?: Record<string, string>
  }) {
    super(args.message)
    this.name = 'ApiError'
    this.status = args.status
    this.code = args.code
    this.requestId = args.requestId ?? ''
    this.fields = args.fields ?? {}
  }
}

/** Local codes, for failures that happen before an envelope exists. */
export const ERR_UNREACHABLE = 'UPSTREAM_UNREACHABLE'
export const ERR_TIMEOUT = 'UPSTREAM_TIMEOUT'
export const ERR_MALFORMED = 'UPSTREAM_MALFORMED'

/**
 * parseEnvelope validates an upstream response and returns its data.
 *
 * The status is trusted over the body's own `success` flag only in one direction: a
 * non-2xx is always a failure, even if the body claims otherwise. A 2xx with
 * `success: false` is also a failure, because that is what the flag is for.
 */
export function parseEnvelope<T>(status: number, body: unknown): Parsed<T> {
  if (!isRecord(body)) {
    throw new ApiError({
      status,
      code: ERR_MALFORMED,
      message: 'The service did not answer with a JSON object.',
    })
  }

  const meta = isRecord(body.meta) ? body.meta : {}
  const requestId = str(meta.requestId)
  const timestamp = str(meta.timestamp)

  const failed = status < 200 || status >= 300 || body.success === false
  if (failed) {
    const info = isRecord(body.error) ? body.error : {}
    throw new ApiError({
      status,
      // A body without an error code on a failing status is itself malformed; saying so
      // is more useful than inventing INTERNAL_ERROR and hiding it.
      code: str(info.code) || ERR_MALFORMED,
      message: str(info.message) || str(body.message) || `The service answered ${status}.`,
      requestId,
      fields: fieldMap(info.fields),
    })
  }

  return {
    data: body.data as T,
    status,
    message: str(body.message),
    requestId,
    timestamp,
    pagination: pagination(meta.pagination),
  }
}

function pagination(raw: unknown): Pagination | null {
  if (!isRecord(raw)) return null
  const page = num(raw.page)
  const limit = num(raw.limit)
  const totalItems = num(raw.totalItems)
  const totalPages = num(raw.totalPages)
  if (page === null || limit === null || totalItems === null || totalPages === null) return null
  return { page, limit, totalItems, totalPages }
}

function fieldMap(raw: unknown): Record<string, string> {
  if (!isRecord(raw)) return {}
  const out: Record<string, string> = {}
  for (const [key, value] of Object.entries(raw)) {
    if (typeof value === 'string') out[key] = value
  }
  return out
}

export function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

/** str narrows to a string, or empty. Never renders "undefined" into the UI. */
export function str(value: unknown): string {
  return typeof value === 'string' ? value : ''
}

/**
 * num narrows to a finite number, or null.
 *
 * Null rather than 0, always. Both services use null to mean "no limit" or "not
 * measured" — `maxTokensPerUserDay: null` is an uncapped user, and 0 would render as a
 * person who may spend nothing. The two must never collapse into each other.
 */
export function num(value: unknown): number | null {
  return typeof value === 'number' && Number.isFinite(value) ? value : null
}

/** bool narrows to a boolean, or null — so "absent" is distinguishable from false. */
export function bool(value: unknown): boolean | null {
  return typeof value === 'boolean' ? value : null
}

/** arr narrows to an array of records, which is what every list route returns. */
export function arr(value: unknown): Record<string, unknown>[] {
  if (!Array.isArray(value)) return []
  return value.filter(isRecord)
}
