'use client'

// The left rail: five places, who is signed in, and the way out.
//
// Fixed width and permanently open from 768px up. An ops console is a room you leave a window
// open on; a rail that hides itself on a desktop is a rail you have to remember the shape of.
// Five rows is the whole navigable surface of this console, and they are in the order you need
// them during an incident rather than alphabetically: the deck, then the things the deck
// points at.
//
// Below 768px it slides, because there it is not a choice. 240px of a 390px screen is 62% of
// the window spent on navigation, and the deck's panels were left 150px to render eight
// columns of figures into. So under the breakpoint the rail leaves the flow entirely and
// translates off the left edge, over the content rather than beside it — Rakazo's own answer,
// down to the backdrop you tap to dismiss it.
//
// Rakazo's sidebar composition too — its own surface, the wordmark at the top, rounded rows
// that fill when current, the account at the foot — because the shape carries information a
// hairline list does not: the rail is a different kind of surface from the screen beside it,
// and the current row is a place you are rather than a link you visited.
//
// The chat row wears the bot's face instead of a glyph. It is the one row that goes somewhere
// you talk to something.

import { useState } from 'react'
import Link from 'next/link'
import { usePathname, useRouter } from 'next/navigation'
import { AGENT_COLOR, BotAvatar, Wordmark } from './bot-avatar'
import { AuditIcon, AuthorityIcon, DeckIcon, GoalsIcon, SignOutIcon } from './icons'
import { call } from '@/lib/client'

const ROWS: readonly { href: string; label: string; icon: 'deck' | 'goals' | 'audit' | 'chat' | 'authority' }[] = [
  { href: '/', label: 'Deck', icon: 'deck' },
  { href: '/goals', label: 'Goals', icon: 'goals' },
  { href: '/audit', label: 'Audit', icon: 'audit' },
  { href: '/chat', label: 'Chat', icon: 'chat' },
  { href: '/authority', label: 'Authority', icon: 'authority' },
]

function RowIcon({ kind }: { kind: (typeof ROWS)[number]['icon'] }) {
  if (kind === 'deck') return <DeckIcon />
  if (kind === 'goals') return <GoalsIcon />
  if (kind === 'audit') return <AuditIcon />
  if (kind === 'authority') return <AuthorityIcon />
  return <BotAvatar color={AGENT_COLOR} size={22} />
}

/** isCurrent treats a section as current for its own page and everything under it. */
function isCurrent(pathname: string, href: string): boolean {
  if (href === '/') return pathname === '/'
  return pathname === href || pathname.startsWith(`${href}/`)
}

/** initials is the account disc's two letters, from the name if there is one. */
function initials(displayName: string, email: string): string {
  const source = displayName.trim() === '' ? email : displayName.trim()
  const parts = source.split(/[\s.@_-]+/).filter((part) => part !== '')
  const letters = parts.slice(0, 2).map((part) => part[0] ?? '')
  return letters.join('').toUpperCase()
}

type Props = {
  /** The signed-in person, rendered at the foot. Read on the server; never a credential. */
  viewer: { displayName: string; email: string }
  /** Drawn open below the desktop breakpoint. Ignored at and above it, where the rail is static. */
  open: boolean
  /** Called when a row is followed, so a phone does not land on the next screen behind the rail. */
  onClose: () => void
}

export function Rail({ viewer, open, onClose }: Props) {
  const pathname = usePathname()
  const router = useRouter()
  const [leaving, setLeaving] = useState(false)

  const handleSignOut = async () => {
    setLeaving(true)
    try {
      await call('/api/session', 'DELETE')
    } catch {
      // The handler clears the cookie whether or not core answered, so there is nothing
      // to report and nothing to retry — leaving is the point.
    }
    // A full navigation rather than a client push: the console's server layout decides
    // where a request with no session goes, and this makes it decide again.
    window.location.assign('/signin')
    router.refresh()
  }

  const name = viewer.displayName === '' ? viewer.email : viewer.displayName

  return (
    <nav
      aria-label="Console"
      // `md:static md:translate-x-0` is what makes `open` a phone-only concern: above the
      // breakpoint the rail is back in the flow and the transform is overridden, so a drawer
      // left open at 390px and then widened needs no resize listener to put itself away.
      className={`absolute inset-y-0 left-0 z-40 flex w-rail shrink-0 flex-col border-r border-border bg-sidebar px-3 pb-3 pt-4 transition-transform duration-200 md:static md:translate-x-0 ${
        open ? 'translate-x-0' : '-translate-x-full'
      }`}
    >
      <div className="px-1.5 pb-4">
        <Wordmark />
      </div>

      <ul className="flex flex-col gap-0.5">
        {ROWS.map((row) => {
          const current = isCurrent(pathname, row.href)
          return (
            <li key={row.href}>
              <Link
                href={row.href}
                onClick={onClose}
                aria-current={current ? 'page' : undefined}
                className={`flex h-nav items-center gap-2.5 rounded-lg px-2.5 text-body transition-colors ${
                  current
                    ? 'bg-sidebar-accent text-foreground'
                    : 'text-muted-foreground hover:bg-sidebar-accent hover:text-foreground'
                }`}
              >
                <RowIcon kind={row.icon} />
                {row.label}
              </Link>
            </li>
          )
        })}
      </ul>

      <div className="mt-auto flex flex-col gap-0.5">
        <div className="flex items-center gap-2.5 rounded-lg px-2.5 py-2" title={viewer.email}>
          <span
            aria-hidden
            className="grid h-7 w-7 shrink-0 place-items-center rounded-full bg-accent text-label font-medium text-muted-foreground"
          >
            {initials(viewer.displayName, viewer.email)}
          </span>
          <span className="min-w-0 truncate text-table text-foreground">{name}</span>
        </div>

        <button
          type="button"
          onClick={() => void handleSignOut()}
          disabled={leaving}
          className="flex h-nav items-center gap-2.5 rounded-lg px-2.5 text-body text-muted-foreground transition-colors hover:bg-sidebar-accent hover:text-foreground disabled:opacity-40"
        >
          <SignOutIcon />
          {leaving ? 'Signing out' : 'Sign out'}
        </button>
      </div>
    </nav>
  )
}
