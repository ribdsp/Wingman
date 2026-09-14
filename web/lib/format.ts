// Rendering numbers and times, deterministically.
//
// Two rules run through everything here, and both come from the services' own semantics:
//
//  1. Absent is not zero. `null` renders as an em dash. Core sends
//     maxTokensPerUserDay: null for an uncapped user and the engine omits an evaluation
//     for a goal it has never assessed; showing 0 for either would be a lie in the
//     dangerous direction — "may spend nothing" and "catastrophically behind".
//  2. Unreadable is not zero either, and it is not absent. A ledger that cannot be read
//     stops a run (`budget_unreadable`), so the console says so in words rather than
//     rendering a number nobody has.

/** What a number the console has no value for looks like. */
export const ABSENT = '—'

const NUMBER = new Intl.NumberFormat('en-US')
const COMPACT = new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 })
const MONEY = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
})
const MONTHS = ['Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun', 'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec']

/** count renders a whole number with separators, or the em dash. */
export function count(value: number | null): string {
  return value === null ? ABSENT : NUMBER.format(Math.round(value))
}

/** compact renders a large number for a heading: 42,000,000 becomes 42M. */
export function compact(value: number | null): string {
  return value === null ? ABSENT : COMPACT.format(value)
}

/**
 * ratio renders a pace or progress figure to two places.
 *
 * Two, because the engine's own reason strings quote two and a console that rounded to
 * one would disagree with the audit log about whether a goal was at 0.95 or 0.951.
 */
export function ratio(value: number | null): string {
  return value === null ? ABSENT : value.toFixed(2)
}

/** percent renders an elapsed or progress ratio the way the engine's reasons do. */
export function percent(value: number | null): string {
  return value === null ? ABSENT : `${Math.round(value * 100)}%`
}

/**
 * PaceState is how the deck colours one row.
 *
 * `unknown` exists so a goal with no evaluation is visibly unassessed rather than
 * silently green. It is the console's counterpart to the engine refusing to read a stale
 * sample as on-track.
 */
export type PaceState = 'ahead' | 'ontrack' | 'behind' | 'unknown'

/**
 * paceState classifies a pace ratio.
 *
 * The boundary is the engine's: on-track means pace at or above 1. `ahead` is a display
 * distinction only — the engine has no such verdict, and nothing here decides anything.
 */
export function paceState(paceRatio: number | null): PaceState {
  if (paceRatio === null) return 'unknown'
  if (paceRatio >= 1.1) return 'ahead'
  if (paceRatio >= 1) return 'ontrack'
  return 'behind'
}

/**
 * Reading is a value plus how sure the console is of it, which is what the status strip
 * needs: a number, an em dash, or the word for "the ledger did not answer".
 */
export type Reading = {
  text: string
  state: 'value' | 'absent' | 'unreadable'
}

/**
 * ledgerReading renders today's spend from core's /v1/me/ledger.
 *
 * isReadable false must never render as 0. A run whose ledger cannot be read stops
 * rather than continuing, and the console showing "0 today" would suggest the opposite:
 * plenty of budget left.
 */
export function ledgerReading(ledger: {
  isReadable: boolean | null
  tokensToday: number | null
}): Reading {
  if (ledger.isReadable === false) return { text: 'unreadable', state: 'unreadable' }
  if (ledger.isReadable === null || ledger.tokensToday === null) {
    return { text: ABSENT, state: 'absent' }
  }
  return { text: NUMBER.format(ledger.tokensToday), state: 'value' }
}

/**
 * capReading renders one of core's run limits.
 *
 * null is uncapped and says so. Zero is not: in core, zero means unset and the floor
 * applies, so a zero arriving here is a bug worth seeing rather than smoothing over.
 */
export function capReading(value: number | null): Reading {
  if (value === null) return { text: 'uncapped', state: 'absent' }
  return { text: NUMBER.format(value), state: 'value' }
}

/**
 * money renders an amount with the currency the engine recorded beside it.
 *
 * Deliberately not Intl's currency style. The engine takes the currency as free text from
 * whoever filed the request, and Intl throws on a code it does not recognise — a console
 * that crashed a panel over a three-letter string would take the approval queue down with
 * it. Two decimal places always, because a spend of 1200 and one of 1200.50 are compared by
 * eye in a column.
 */
export function money(amount: number | null, currency: string): string {
  if (amount === null) return ABSENT
  const figure = MONEY.format(amount)
  return currency === '' ? figure : `${figure} ${currency.toUpperCase()}`
}

/**
 * stamp renders an RFC 3339 timestamp in the offset the service sent it in.
 *
 * Deliberately string-based rather than via Date: the services render timestamps in the
 * instance's configured zone, and converting to the viewer's local time would mean the
 * console and the audit log disagree about when something happened. A laptop in another
 * country should read the same clock the server wrote.
 */
export function stamp(iso: string, withSeconds = false): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):(\d{2})/.exec(iso)
  if (!match) return ABSENT
  const [, , month, day, hour, minute, second] = match
  const name = MONTHS[Number(month) - 1] ?? ABSENT
  const time = withSeconds ? `${hour}:${minute}:${second}` : `${hour}:${minute}`
  return `${day} ${name} ${time}`
}

/** dayStamp is the date alone, for grouping rows under a heading. */
export function dayStamp(iso: string): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})/.exec(iso)
  if (!match) return ABSENT
  const [, year, month, day] = match
  return `${day} ${MONTHS[Number(month) - 1] ?? ABSENT} ${year}`
}

/**
 * clock is the time alone, for rows that already sit under a dayStamp heading.
 *
 * The pair exists so a grouped table states each date once. Repeating `13 Sep` down forty
 * rows of one day costs the width of the column it is in and reads as though the date were
 * the varying part, when the time is what the reader is scanning for.
 *
 * Same string-based reasoning as stamp: the zone is the one the service wrote in.
 */
export function clock(iso: string): string {
  const match = /T(\d{2}):(\d{2}):(\d{2})/.exec(iso)
  if (!match) return ABSENT
  const [, hour, minute, second] = match
  return `${hour}:${minute}:${second}`
}

/**
 * ago renders how long since a timestamp, coarsely.
 *
 * Coarse on purpose: the question a deck answers is "is this feed alive", and a
 * ticking second counter invites reading precision that a 4-second poll does not have.
 */
export function ago(iso: string, now: number): string {
  const at = Date.parse(iso)
  if (Number.isNaN(at)) return ABSENT
  const seconds = Math.floor((now - at) / 1000)
  if (seconds < 0) return 'just now'
  if (seconds < 45) return 'just now'
  if (seconds < 90) return '1m ago'
  const minutes = Math.round(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  return `${days}d ago`
}

/**
 * remaining renders a countdown, used for held operator authority and for an approval's
 * expiry. Returns null once it has lapsed, so the caller renders the lapsed state rather
 * than "0m".
 */
export function remaining(expiresAt: number, now: number): string | null {
  const ms = expiresAt - now
  if (ms <= 0) return null
  const minutes = Math.floor(ms / 60_000)
  if (minutes < 1) return 'under a minute'
  if (minutes < 60) return `${minutes}m`
  const hours = Math.floor(minutes / 60)
  const rest = minutes % 60
  return rest === 0 ? `${hours}h` : `${hours}h ${rest}m`
}

/** millis renders a duration a run or a step took. */
export function millis(value: number | null): string {
  if (value === null) return ABSENT
  if (value < 1000) return `${Math.round(value)}ms`
  if (value < 60_000) return `${(value / 1000).toFixed(1)}s`
  const minutes = Math.floor(value / 60_000)
  const seconds = Math.round((value % 60_000) / 1000)
  return `${minutes}m ${seconds}s`
}

/**
 * words turns a snake_case enum from either service into something readable, without
 * losing it: stop reasons, decisions and outcomes are the vocabulary the docs use, so
 * `budget_unreadable` becomes `budget unreadable` and not something invented.
 */
export function words(value: string): string {
  return value.replaceAll('_', ' ')
}

/**
 * How much longer than the budget an id may run before cutting it buys anything.
 *
 * Without it, `kill-switch` — the kill switch's subject id, and the one audit row an
 * operator most wants to find — renders as `kill-swi…` to save two characters in a column
 * that is already the narrowest on the row.
 */
const SHORT_SLACK = 4

/**
 * short trims an id for a dense table, keeping enough of it to recognise the row.
 *
 * Most ids here are uuids, and a uuid's first eight characters are enough to tell one row
 * from another and enough to grep a service's log with. The rest is column width.
 *
 * The cut is always marked. Subject ids are not all uuids — `kill-switch` is one, a
 * credential name is another — and `kill-switch` rendered as `kill-swi` reads as an id that
 * is genuinely called that. A reader cannot tell a value from a truncation, which is the
 * same failure as the clipped column this table used to have: one ellipsis costs a
 * character and says the rest exists. Detail lines are truncated the same way.
 */
export function short(id: string, length = 8): string {
  if (id.length <= length + SHORT_SLACK) return id
  return `${id.slice(0, length)}…`
}

/** How much of an audit row's detail fits on one line before it stops being scannable. */
const DETAIL_LIMIT = 180

/**
 * detailLine flattens an audit row's `detail` to one line.
 *
 * The engine stores detail as JSON and emits it as JSON — a decision's numbers, a policy's
 * name, an actor's note — so its shape is not fixed and cannot be typed. Flattened to
 * `key=value` pairs rather than pretty-printed, because the audit log is read as a table and
 * a row that grew to eight lines would push the next event off the screen.
 *
 * Truncated rather than hidden. The full record is one API call away; what a person scanning
 * needs is enough to recognise which event this is.
 */
export function detailLine(detail: unknown): string {
  const line = flatten(detail)
  return line.length <= DETAIL_LIMIT ? line : `${line.slice(0, DETAIL_LIMIT)}…`
}

function flatten(value: unknown): string {
  if (value === null || value === undefined) return ''
  if (typeof value === 'string') return value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  if (Array.isArray(value)) return value.map(flatten).join(', ')
  if (typeof value === 'object') {
    return Object.entries(value)
      .map(([name, held]) => `${name}=${scalar(held)}`)
      .join('  ')
  }
  return ''
}

/** One level deep, then JSON. A nested object on an audit row is rare and stays readable. */
function scalar(value: unknown): string {
  if (value === null || value === undefined) return ABSENT
  if (typeof value === 'string') return value === '' ? ABSENT : value
  if (typeof value === 'number' || typeof value === 'boolean') return String(value)
  try {
    return JSON.stringify(value) ?? ABSENT
  } catch {
    // A cycle cannot come out of JSON the service sent, but this runs on every audit row
    // and a thrown formatter would blank the panel rather than one cell.
    return ABSENT
  }
}
