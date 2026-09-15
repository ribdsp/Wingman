// The console's visual vocabulary, and all of it.
//
// Six things: a panel, an instrument reading, a strip chip, a coloured word, an empty state
// and a failure note. Everything on every screen is built from these, which is why the
// screens stay legible as they gain panels — a new panel cannot invent a new way of looking.
//
// A panel is a card: its own fill, one hairline, a 12px corner. Rakazo's surface model, and
// adopted for its reason rather than its look — a reading that sits on a surface of its own
// is one thing you can take in, where a grid of hairline-separated rows is a spreadsheet and
// gets read like one. The density inside a card is unchanged: 28px rows, 13px figures, no
// decorative padding. Friendly edges, dense contents.
//
// Presentational only — no hooks, no state, no 'use client'. Server components render these
// directly, and the polled panels wrap them.

import type { ReactNode } from 'react'
import { words } from '@/lib/format'
import { MARK, TEXT, type Tone } from '@/lib/tone'

/** slug turns a panel label into an id, so its heading can label its section. */
function slug(label: string): string {
  return label.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')
}

type PanelProps = {
  label: string
  /** A single figure beside the label — how many are waiting, how many are behind. */
  count?: ReactNode
  /** Right-aligned in the header. A filter, a button, a timestamp. */
  action?: ReactNode
  /** Rendered under the header when the last read failed. */
  error?: string
  children: ReactNode
}

/**
 * Panel is one instrument on the deck.
 *
 * The header is a single 38px row: label, figure, and whatever acts on it. The card carries
 * the separation, so panels stack with a gap between them and each one reads as a reading of
 * its own rather than another band in a grid.
 */
export function Panel({ label, count, action, error, children }: PanelProps) {
  const id = slug(label)
  return (
    <section
      aria-labelledby={`${id}-label`}
      className="rounded-xl border border-border bg-card"
    >
      <header className="flex min-h-nav items-center gap-3 px-4 pt-1">
        <h2 id={`${id}-label`} className="label">
          {label}
        </h2>
        {count !== undefined && <span className="num text-table text-muted-foreground">{count}</span>}
        <div className="ml-auto flex items-center gap-3">{action}</div>
      </header>
      {error !== '' && error !== undefined && <Note>{error}</Note>}
      {/*
       * overflow-x-auto because these tables have a floor: eight or nine columns of
       * `whitespace-nowrap` figures. Below roughly a 1200px window the pace table cannot
       * fit, and the alternative to scrolling it is what this used to do — clip the last two
       * columns off the right edge with no indication they existed. A reading that is one
       * scroll away is recoverable; a reading you cannot know is missing is not.
       */}
      <div className="overflow-x-auto px-4 pb-4">{children}</div>
    </section>
  )
}

type InstrumentProps = {
  label: string
  value: ReactNode
  tone?: Tone
  /** One line under the reading: the unit, the caveat, when it was taken. */
  sub?: ReactNode
}

/**
 * Instrument is a single figure, read at a glance from across a room.
 *
 * Deliberately the only large type in the console. Scale is the hierarchy here — there are
 * no headings competing with it, because a heading is not a reading.
 */
export function Instrument({ label, value, tone = 'quiet', sub }: InstrumentProps) {
  return (
    <div className="flex flex-col gap-0.5">
      <span className="label">{label}</span>
      <span className={`num text-instrument leading-none ${TEXT[tone]}`}>{value}</span>
      {sub !== undefined && <span className="text-table text-faint">{sub}</span>}
    </div>
  )
}

/**
 * Word renders one of the services' snake_case verdicts, coloured by tone.
 *
 * The underscores are turned into spaces and nothing else: `budget_unreadable` becomes
 * "budget unreadable" and stays the word the audit log and the docs use. A console that
 * rewrote it as "Budget could not be read" would be inventing a second vocabulary for the
 * same event.
 */
export function Word({ value, tone }: { value: string; tone: Tone }) {
  if (value === '') return <span className="text-faint">—</span>
  return <span className={TEXT[tone]}>{words(value)}</span>
}

/**
 * Mark is a 6px dot carrying a tone, and nothing else.
 *
 * A dot rather than the 2px vertical tick this used to be: every one of its six call sites
 * puts it immediately before a word, and at 13px a thin upright bar beside text reads as a
 * stray pipe character rather than as a status light. A round dot is read as a light.
 */
export function Mark({ tone }: { tone: Tone }) {
  return <span aria-hidden className={`inline-block size-1.5 rounded-full align-middle ${MARK[tone]}`} />
}

/**
 * Chip is one reading on the status strip: a caption, a figure, one rounded surface.
 *
 * The strip is the only place in the console where four unrelated readings sit on one line,
 * and a pill each is what keeps them from reading as one sentence. Its own component rather
 * than a class string because the strip is a client component and this is not — nothing here
 * needs to ship to the browser.
 */
export function Chip({ label, children }: { label?: string; children: ReactNode }) {
  return (
    <span className="flex items-center gap-2 rounded-full bg-card px-3 py-1">
      {label !== undefined && <span className="label">{label}</span>}
      {children}
    </span>
  )
}

/**
 * Note is a failure line: why what is above it may be stale.
 *
 * The stopped colour, and it stays next to the reading rather than replacing it. The panel
 * keeps showing the last thing it knew, because during an incident the last known reading
 * plus "this read failed" is more use than a blank space.
 */
export function Note({ children }: { children: ReactNode }) {
  return (
    <p role="status" className="px-4 pb-1 text-table text-destructive">
      {children}
    </p>
  )
}

/** Empty is what a panel says when there is genuinely nothing, which is often good news. */
export function Empty({ children }: { children: ReactNode }) {
  return <p className="py-2 text-table text-faint">{children}</p>
}

/**
 * Field is a label and a value on one line, for the detail pages.
 *
 * Used inside a <dl>, because that is what these are: a term and its value. Screen readers
 * get the pairing for free and the markup says what the layout is doing.
 */
export function Field({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex items-baseline gap-2 border-b border-border py-1">
      {/* Narrower below sm: 160px of a 342px card spent on an 11px caption leaves the value
          it labels scrolling sideways. 112px still clears the longest of them. */}
      <dt className="label w-28 shrink-0 sm:w-40">{label}</dt>
      <dd className="text-table">{children}</dd>
    </div>
  )
}

/** Num wraps a figure so every number in the console is tabular and monospaced. */
export function Num({ children }: { children: ReactNode }) {
  return <span className="num">{children}</span>
}

/**
 * Id renders an identifier: mono, dim, and never the thing your eye lands on first.
 *
 * `whitespace-nowrap` because an id broken across two lines is neither readable nor
 * copyable, and every id in this console exists to be carried somewhere else — into a
 * service's access log, into a URL, into a support message. It also sets the cell's
 * min-content width, so a table holding one keeps a column wide enough for it and the
 * panel scrolls, instead of shredding the id and inflating every row to match.
 */
export function Id({ children }: { children: ReactNode }) {
  return <span className="num whitespace-nowrap text-faint">{children}</span>
}

/**
 * The table classes, in one place.
 *
 * Class strings rather than components: a table needs its own <thead>/<tbody> structure to
 * stay a table for a screen reader, and wrapping every cell in a component to add one class
 * would cost that without buying anything.
 */
/**
 * `pr-4 last:pr-0` is load-bearing, not spacing taste. These tables are dense and mostly
 * `whitespace-nowrap`, so with no gutter two adjacent cells render as one word: an amount
 * runs into who asked for it (`750.00 USDbot:orbit-growth`), a step number into its kind
 * (`2tool_call`), a token count into its timestamp. The last column is flush right because it
 * is the row's controls or its right-aligned figure and the panel already has its own padding.
 */
export const TABLE = 'w-full border-collapse text-table'
export const TH = 'label border-b border-border pr-4 pb-1 text-left align-bottom font-medium last:pr-0'
export const TD = 'h-row border-b border-border/60 pr-4 align-middle whitespace-nowrap last:pr-0'
/** For the one column per table that may wrap and take the remaining width. */
/**
 * `min-w-56` is the floor under that column, and it is why these tables survive a phone.
 *
 * Every other cell is `whitespace-nowrap`, so when the window narrows this is the only column
 * that can give — and it gives all of it: a reason, a brief or a three-word goal title
 * squeezes to 40px and stacks into six lines, and the row stops being a row. With a floor the
 * panel's own `overflow-x-auto` takes over instead, which is recoverable. A genuinely long
 * value still wraps, just not to a column of one word.
 */
export const TD_WIDE = 'h-row min-w-56 border-b border-border/60 pr-4 align-middle last:pr-0'

/**
 * Button is the console's only button shape.
 *
 * A 10px corner, a hairline, no fill until hover — the same shape as everything else here,
 * one step tighter than a panel because it is a smaller thing. `intent` exists because two
 * buttons in this console change the world — resolving an approval and engaging the kill
 * switch — and those two are allowed to look like it. Everything else is quiet.
 */
export function buttonClass(intent: 'quiet' | 'act' | 'stop' = 'quiet'): string {
  // `whitespace-nowrap` because every button here is two or three words and `h-row` is a
  // fixed 28px: on a 390px status strip "Engage kill switch" wrapped to two lines and burst
  // out of its own border. A label that does not fit belongs on a shorter button.
  const base =
    'inline-flex h-row items-center gap-1.5 whitespace-nowrap rounded-md border px-2.5 text-table transition-colors disabled:cursor-not-allowed disabled:opacity-40'
  if (intent === 'act') {
    return `${base} border-warning-dim text-warning hover:bg-warning-dim/25`
  }
  if (intent === 'stop') {
    return `${base} border-destructive-dim text-destructive hover:bg-destructive-dim/25`
  }
  return `${base} border-input text-muted-foreground hover:bg-accent hover:text-foreground`
}

/** The input shape, likewise once. */
export const INPUT =
  'h-row w-full rounded-md border border-input bg-background px-2.5 text-table text-foreground placeholder:text-faint focus:border-faint'
