'use client'

// The console shell: the rail, the status strip, and the halted state.
//
// Everything on this strip is a poll. Core has no streaming endpoint — deliberately — so
// "what is true now" is a question asked on an interval, and the strip is where the answer
// lives so that no page has to ask it again.
//
// Three deliberate behaviours:
//
//   The kill switch fails closed here too. A read that fails becomes SWITCH_UNREADABLE,
//   which is engaged, so the console goes to its halted state when the engine stops
//   answering rather than continuing to render a released brake it cannot see.
//   Every other reading keeps its last value and says the read failed beside it.
//
//   Halted is a state of the whole console, not a badge. `data-halted` drains the attention
//   colour out of the palette, because nothing is waiting on a human while everything is
//   stopped, and the top rule turns red and breathes.
//
//   The server rendered this strip's first values on this request, so the first poll waits
//   out an interval instead of firing immediately.

import type { ReactNode } from 'react'
import { useState } from 'react'
import { AuthorityChip, isHeld } from './authority'
import { Chip, Mark } from './chrome'
import { MenuIcon } from './icons'
import { KillSwitch } from './kill-switch'
import { Rail } from './rail'
import { get, message, useNow, usePoll } from '@/lib/client'
import { ledgerReading } from '@/lib/format'
import type { Shell } from '@/lib/deck'
import {
  type LedgerReadingRaw,
  readLedger,
  readSwitch,
  SWITCH_UNREADABLE,
  type SwitchReading,
} from '@/lib/types'

/**
 * Strip is the server's Shell without the poll interval, which the client already has.
 *
 * Derived from Shell rather than written out again: adding a reading to the server's strip
 * should stop this file compiling until the client polls it too.
 */
export type Strip = Omit<Shell, 'pollIntervalMs'>

async function settle<T>(fallback: T, load: () => Promise<T>): Promise<{ value: T; error: string }> {
  try {
    return { value: await load(), error: '' }
  } catch (thrown) {
    return { value: fallback, error: message(thrown) }
  }
}

/**
 * loadStrip re-reads the four things the strip shows, and never throws.
 *
 * Not throwing is the point: usePoll keeps its last value when a load throws, and for the
 * kill switch that would mean holding on to "released" while the engine is unreachable.
 * Each read degrades on its own instead, the switch to engaged-and-unreadable.
 */
async function loadStrip(signal: AbortSignal): Promise<Strip> {
  const [flag, open, ledger, operator] = await Promise.all([
    settle<SwitchReading>(SWITCH_UNREADABLE, async () =>
      readSwitch((await get<unknown>('/api/engine/v1/flags/kill-switch', signal)).data),
    ),
    settle<number | null>(null, async () => {
      const parsed = await get<unknown>('/api/engine/v1/approvals?openOnly=true&limit=1', signal)
      return parsed.pagination?.totalItems ?? null
    }),
    settle<LedgerReadingRaw>({ isReadable: null, tokensToday: null }, async () =>
      readLedger((await get<unknown>('/api/core/v1/me/ledger', signal)).data),
    ),
    settle<number | null>(null, async () => {
      const parsed = await get<{ held: boolean; expiresAt: number | null }>('/api/operator', signal)
      return parsed.data.expiresAt
    }),
  ])

  return {
    switch: flag.value,
    switchError: flag.error,
    openApprovals: open.value,
    approvalsError: open.error,
    ledger: ledger.value,
    ledgerError: ledger.error,
    operatorExpiresAt: operator.value,
  }
}

type Props = {
  viewer: { displayName: string; email: string }
  /** Read on the server for this request, so the first paint is never a blank instrument. */
  initial: Strip
  pollIntervalMs: number
  children: ReactNode
}

export function DeckShell({ viewer, initial, pollIntervalMs, children }: Props) {
  const poll = usePoll<Strip>(loadStrip, pollIntervalMs, initial)
  const now = useNow()
  // Phone-only, and deliberately not persisted: the rail's default state is closed on every
  // arrival, because on a 390px screen the thing you came for is the panel, not the menu.
  const [navOpen, setNavOpen] = useState(false)

  const strip = poll.value
  const halted = strip.switch.engaged
  const operator = isHeld(strip.operatorExpiresAt, now)
  const spend = ledgerReading(strip.ledger)
  const waiting = strip.openApprovals
  const notes = [strip.switchError, strip.approvalsError, strip.ledgerError].filter((t) => t !== '')

  return (
    <div
      data-halted={halted ? 'true' : 'false'}
      // `relative` positions the rail's drawer against this box rather than the page, and
      // `overflow-hidden` is what clips it while it is translated off the left edge. `h-dvh`
      // rather than `h-screen`: on a phone `100vh` is the window with the browser's own bars
      // pretended away, which puts the composer under them.
      className="relative flex h-dvh overflow-hidden bg-background text-foreground"
    >
      <Rail viewer={viewer} open={navOpen} onClose={() => setNavOpen(false)} />

      {/* Tap-anywhere-else to dismiss. A button rather than a div so it is reachable by
          keyboard and announced, and only below the breakpoint, where the rail is over the
          content instead of beside it. It starts where the rail ends rather than at inset-0:
          the rail sits above it either way, but a backdrop whose own centre is behind the
          rail is a hit target that lies about where it is. */}
      {navOpen && (
        <button
          type="button"
          aria-label="Close navigation"
          onClick={() => setNavOpen(false)}
          className="absolute inset-y-0 right-0 left-rail z-30 bg-background/70 md:hidden"
        />
      )}

      <div className="flex min-w-0 flex-1 flex-col">
        {/* The top rule is the one piece of ambient state: hairline normally, red and
            breathing while the engine is halted. */}
        <div aria-hidden className={`h-px shrink-0 ${halted ? 'breathe bg-destructive' : 'bg-border'}`} />

        <header
          aria-label="Status"
          className="flex shrink-0 flex-wrap items-center gap-x-3 gap-y-2 border-b border-border px-4 py-2.5"
        >
          <button
            type="button"
            aria-label="Open navigation"
            aria-expanded={navOpen}
            onClick={() => setNavOpen(true)}
            className="-ms-1.5 grid size-8 shrink-0 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground md:hidden"
          >
            <MenuIcon />
          </button>

          <Chip>
            <Mark tone={halted ? 'stop' : 'ok'} />
            <span className={`text-body ${halted ? 'text-destructive' : 'text-success'}`}>
              {halted ? 'halted' : 'live'}
            </span>
            {!strip.switch.readable && (
              <span className="text-table text-destructive">· engine did not answer</span>
            )}
            {strip.switch.readable && halted && strip.switch.reason !== '' && (
              <span className="text-table text-muted-foreground">· {strip.switch.reason}</span>
            )}
          </Chip>

          <KillSwitch reading={strip.switch} isOperator={operator} onChanged={poll.refresh} />

          <div className="ml-auto flex flex-wrap items-center gap-3">
            <Chip label="waiting">
              <span className={`num text-body ${waiting !== null && waiting > 0 ? 'text-warning' : 'text-muted-foreground'}`}>
                {waiting === null ? '—' : waiting}
              </span>
            </Chip>

            <Chip label="tokens today">
              <span
                className={`num text-body ${spend.state === 'unreadable' ? 'text-destructive' : 'text-muted-foreground'}`}
              >
                {spend.text}
              </span>
            </Chip>

            <Chip>
              <AuthorityChip expiresAt={strip.operatorExpiresAt} now={now} />
            </Chip>

            {/* The only moving thing on the screen, and it moves for two seconds at a time
                to say the poll is alive. A spinner would imply something is being waited for. */}
            <span
              aria-hidden
              className="relative block h-px w-8 shrink-0 overflow-hidden bg-border"
              title="polling"
            >
              {poll.loading && <span className="sweep absolute inset-y-0 left-0 w-2 bg-faint" />}
            </span>
          </div>
        </header>

        {notes.length > 0 && (
          <p role="status" className="shrink-0 border-b border-border px-4 py-1 text-table text-destructive">
            {notes.join(' · ')}
          </p>
        )}

        {/* `relative` is load-bearing, not decoration. Every table here carries an sr-only
            <caption> and the composer an sr-only <label>, and Tailwind's sr-only is
            `position: absolute` with no offsets — so with a static main its containing block
            is the page, not this scroller, and overflow-y-auto does not clip it. One such
            label sitting 1362px down the deck's content stretched the document to 1363px
            behind an h-screen shell: the whole console could be scrolled up by 463px, taking
            the status strip off the top and leaving dead ink under a floating rail. Making
            this the containing block puts those boxes back inside the scroller that already
            clips them.

            `inert` while the drawer is open so Tab does not walk into the panels behind the
            backdrop. The header above stays live, because the toggle that closes the drawer
            is in it. */}
        <main inert={navOpen} className="relative min-w-0 flex-1 overflow-y-auto">
          {/* The gutter and the gap between panels live here rather than on every screen, so a
              new page cannot arrive with its own spacing. Panels carry their own padding
              inside; this is only the air around them, which is what a card needs and a
              hairline-separated band did not. */}
          <div className="flex flex-col gap-3 p-3">{children}</div>
        </main>
      </div>
    </div>
  )
}
