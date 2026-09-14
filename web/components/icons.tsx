// The console's icons, and all of them.
//
// Hand-drawn 24-grid strokes rather than an icon package: seven glyphs do not justify a
// dependency, and a package would ship several hundred more that nothing here renders.
// One size, one stroke weight, `currentColor` throughout, so an icon takes the colour of
// the row it sits in and nothing has to be recoloured twice.
//
// Presentational only, and `aria-hidden` on every one: each icon in this console sits
// beside its own label, so an accessible name here would be the label read twice.

import type { ReactNode } from 'react'

type Props = {
  size?: number
  className?: string
}

function Glyph({ size = 16, className, children }: Props & { children: ReactNode }) {
  return (
    <svg
      aria-hidden
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.7}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={`shrink-0 ${className ?? ''}`}
    >
      {children}
    </svg>
  )
}

/** The deck: a dial, because that is what the deck is. */
export function DeckIcon(props: Props) {
  return (
    <Glyph {...props}>
      <path d="M3.5 17a9.5 9.5 0 1 1 17 0" />
      <path d="M12 13.5 16 9" />
      <circle cx="12" cy="17" r="1.6" />
    </Glyph>
  )
}

/** Goals: a target with a bar to hit. */
export function GoalsIcon(props: Props) {
  return (
    <Glyph {...props}>
      <circle cx="12" cy="12" r="8.5" />
      <circle cx="12" cy="12" r="4" />
      <circle cx="12" cy="12" r="0.6" />
    </Glyph>
  )
}

/** Audit: a log. Lines of record, the last one short because it is still being written. */
export function AuditIcon(props: Props) {
  return (
    <Glyph {...props}>
      <path d="M4.5 6.5h15" />
      <path d="M4.5 12h15" />
      <path d="M4.5 17.5h9" />
    </Glyph>
  )
}

/** Authority: a key held for half an hour. */
export function AuthorityIcon(props: Props) {
  return (
    <Glyph {...props}>
      <circle cx="8.5" cy="8.5" r="4.5" />
      <path d="M11.8 11.8 20 20" />
      <path d="m15 15 2-2" />
    </Glyph>
  )
}

/** The way out. */
export function SignOutIcon(props: Props) {
  return (
    <Glyph {...props}>
      <path d="M10 4.5H6.5a1.5 1.5 0 0 0-1.5 1.5v12a1.5 1.5 0 0 0 1.5 1.5H10" />
      <path d="m15.5 8.5 3.5 3.5-3.5 3.5" />
      <path d="M19 12h-9" />
    </Glyph>
  )
}

/**
 * The rail's toggle, below the desktop breakpoint only.
 *
 * Three rules and not the four-row "hamburger" of a marketing site: the rail it opens has
 * five rows, and a glyph that suggests a list is closer to what is behind it than a stack
 * that suggests a menu of everything.
 */
export function MenuIcon({ size = 18, className }: Props) {
  return (
    <Glyph size={size} className={className}>
      <path d="M4 7h16" />
      <path d="M4 12h16" />
      <path d="M4 17h16" />
    </Glyph>
  )
}

/** Send. The one icon that is a verb. */
export function SendIcon({ size = 18, className }: Props) {
  return (
    <svg
      aria-hidden
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={2}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={`shrink-0 ${className ?? ''}`}
    >
      <path d="M12 19.5V5" />
      <path d="m5.5 11.5 6.5-6.5 6.5 6.5" />
    </svg>
  )
}
