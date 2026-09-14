// Turning Next's searchParams into the two shapes the pages actually want.
//
// A page receives `Record<string, string | string[] | undefined>`, because a URL may repeat a
// name. Both services read each filter once — `c.Query("status")` takes the first value — so
// the console keeps the first and drops the rest rather than inventing a meaning for the
// second. Silently sending `?status=active&status=paused` upstream and rendering the answer to
// one of them is the kind of thing an operator debugs for an hour.
//
// Nothing here validates. The allowlist in lib/proxy-routes.ts owns that, in one place, for
// both the server reads and the browser proxy.

/** A filter as the pages pass it around: one value per name. */
export type Values = Readonly<Record<string, string>>

/**
 * firstValues flattens Next's searchParams, keeping the first value of a repeated name.
 *
 * Empty values are dropped. `?product=` is a name with nothing behind it — from a cleared
 * input, usually — and forwarding it would make the engine filter on the empty string.
 */
export function firstValues(
  params: Readonly<Record<string, string | string[] | undefined>>,
): Values {
  const values: Record<string, string> = {}
  for (const [name, value] of Object.entries(params)) {
    const first = Array.isArray(value) ? value[0] : value
    if (first === undefined || first === '') continue
    values[name] = first
  }
  return values
}

/**
 * queryString rebuilds a query string from a flat record, names sorted.
 *
 * Sorted so the same filter always produces the same string: the reads take it as a cache key
 * in everything but name, and two spellings of one filter would be two entries.
 */
export function queryString(values: Values): string {
  const params = new URLSearchParams()
  for (const name of Object.keys(values).sort()) {
    const value = values[name]
    if (value !== undefined && value !== '') params.set(name, value)
  }
  return params.toString()
}

/**
 * withValue returns the query string for the same filter with one name changed.
 *
 * An empty value removes the name, and `offset` is always dropped: every caller here is
 * changing what is being looked at, and page 4 of the previous filter is not page 4 of this
 * one — it is usually past the end, which renders as "no matching events".
 */
export function withValue(values: Values, name: string, value: string): string {
  const next: Record<string, string> = { ...values }
  delete next.offset
  if (value === '') delete next[name]
  else next[name] = value
  return queryString(next)
}

/**
 * href joins a path and a query string.
 *
 * Trivial, and here rather than inline in five pages because the alternative is five slightly
 * different answers to "what does this link look like when the filter is empty".
 */
export function href(pathname: string, search: string): string {
  return search === '' ? pathname : `${pathname}?${search}`
}

/** A parsed page position: what to ask for, and where the previous page starts. */
export type Window = {
  limit: number
  offset: number
}

/** The page size a list uses when the URL does not name one. */
export const DEFAULT_LIMIT = 50

/**
 * windowOf reads limit and offset out of a filter, with bounds.
 *
 * Bounded here as well as upstream because these two numbers are arithmetic, not just
 * forwarded: a negative or absurd limit would produce a pager that offers pages that cannot
 * exist. The ceiling matches what the services accept.
 */
export function windowOf(values: Values, fallback = DEFAULT_LIMIT): Window {
  return {
    limit: bounded(values.limit, fallback, 1, 200),
    offset: bounded(values.offset, 0, 0, 1_000_000_000),
  }
}

function bounded(raw: string | undefined, fallback: number, low: number, high: number): number {
  if (raw === undefined) return fallback
  const value = Number(raw)
  if (!Number.isInteger(value) || value < low || value > high) return fallback
  return value
}
