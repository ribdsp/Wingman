// The bot's face.
//
// Ported from Rakazo's `packages/ui-web/src/bot-avatar.tsx` (Apache-2.0 — see NOTICE), which
// is the thing worth copying: a glossy bead with a dark visor and two glowing eyes, one size
// parameter, and every dimension derived from it so the same component is a 24px row marker
// and a 44px chat header without a second set of numbers.
//
// Two changes from the original, both so this can render on the server:
//
//   No `useId`. Rakazo's spinning ring is stroked with an SVG gradient, which needs a unique
//   `<defs>` id per instance and therefore a hook. The comet look here comes from the dash gap
//   and the two drop-shadows instead, and the arc is plain white — at the sizes this console
//   uses, the difference is not visible, and the avatar stays a server component with no
//   client bundle and no hydration to get wrong.
//
//   No organic variant. Rakazo ships a second face whose blob shape is generated per identity;
//   this console shows one agent, so a second face would be a choice nobody makes.
//
// The animation is entirely in `app/avatar.css`. Nothing here runs on an interval.

import type { CSSProperties } from 'react'

/**
 * Rakazo's seven bot colours, verbatim.
 *
 * Used to tell one conversation from another in a list. Seven fixed hues rather than a hue
 * generated from the id: a generated hue lands on mud and on colours already carrying meaning
 * in this palette, and these seven were picked to sit on near-black.
 */
export const BOT_COLORS = [
  '#3ec5a8',
  '#f5a03c',
  '#6a6bf5',
  '#9b5cf6',
  '#3b82f6',
  '#f2622a',
  '#d9508a',
] as const

/** The house colour: Wingman's own mark, and any avatar that is the instance rather than a chat. */
export const AGENT_COLOR = BOT_COLORS[0]

/** hash is a stable non-cryptographic digest of a string. Rakazo's, unchanged. */
function hash(value: string): number {
  let out = 0
  for (let i = 0; i < value.length; i += 1) {
    out = (out << 5) - out + value.charCodeAt(i)
    out |= 0
  }
  return Math.abs(out)
}

/** botColor picks one of the seven for an id, so the same conversation is the same colour. */
export function botColor(seed: string): string {
  return BOT_COLORS[hash(seed) % BOT_COLORS.length] as string
}

function shift(hex: string, percent: number): string {
  const clean = hex.replace(/^#/, '')
  if (clean.length !== 6) return hex
  const num = Number.parseInt(clean, 16)
  if (Number.isNaN(num)) return hex
  const step = Math.round((255 * percent) / 100)
  const channel = (value: number) => Math.min(255, Math.max(0, value + step))
  const r = channel(num >> 16)
  const g = channel((num >> 8) & 0xff)
  const b = channel(num & 0xff)
  return `#${((1 << 24) + (r << 16) + (g << 8) + b).toString(16).slice(1)}`
}

type Props = {
  /** The bead's hue. `botColor(chatId)` for a conversation, `AGENT_COLOR` for the instance. */
  color?: string
  size?: number
  /** True while a run is in flight: the eyes track and the ring spins. */
  working?: boolean
  className?: string
}

export function BotAvatar({ color = AGENT_COLOR, size = 38, working = false, className }: Props) {
  const visorW = Math.round(size * 0.68)
  const visorH = Math.round(size * 0.44)
  const eyeW = Math.max(4, Math.round(size * 0.14))
  const eyeH = Math.max(7, Math.round(size * 0.22))
  const eyeRadius = Math.max(2, Math.round(eyeW * 0.5))
  const eyeGap = Math.max(3, Math.round(size * 0.1))

  // The idle blink is seeded from the colour, so two avatars on one screen are out of step.
  const seed = hash(color)
  const idle: CSSProperties = {
    '--wgm-eye-name': `wgm-eyes-idle-${seed % 4}`,
    '--wgm-eye-duration': `${(4.2 + ((seed * 7) % 28) / 10).toFixed(2)}s`,
    '--wgm-eye-easing': 'cubic-bezier(0.4, 0, 0.2, 1)',
    '--wgm-eye-delay': `${(-(((seed * 13) % 45) / 10)).toFixed(2)}s`,
  } as CSSProperties
  const busy: CSSProperties = {
    '--wgm-eye-name': 'wgm-eyes-working',
    '--wgm-eye-duration': '1.4s',
    '--wgm-eye-easing': 'ease-in-out',
    '--wgm-eye-delay': '0s',
  } as CSSProperties

  return (
    <span
      aria-hidden
      data-working={working ? 'true' : 'false'}
      className={`wgm-avatar relative flex shrink-0 select-none items-center justify-center rounded-full ${className ?? ''}`}
      style={{
        width: size,
        height: size,
        background: `radial-gradient(circle at 35% 26%, ${shift(color, 35)}, ${color} 55%, ${shift(color, -40)} 100%)`,
        boxShadow: working
          ? `0 0 0 2px rgba(255,255,255,0.25), 0 0 ${Math.round(size * 0.45)}px ${color}, inset 0 1px 2px rgba(255,255,255,0.6)`
          : `0 2px ${Math.max(4, Math.round(size * 0.15))}px rgba(0,0,0,0.4), inset 0 1px 1.5px rgba(255,255,255,0.4)`,
      }}
    >
      <svg
        className="wgm-avatar-ring pointer-events-none absolute"
        style={{
          inset: -4,
          width: size + 8,
          height: size + 8,
          filter: `drop-shadow(0 0 6px ${color}) drop-shadow(0 0 10px #ffffff)`,
        }}
        viewBox="0 0 48 48"
        fill="none"
      >
        <circle
          cx="24"
          cy="24"
          r="22"
          stroke="#ffffff"
          strokeOpacity="0.92"
          strokeWidth="3.2"
          strokeLinecap="round"
          strokeDasharray="45 80"
        />
        <circle cx="43" cy="24" r="2.8" fill="#ffffff" />
      </svg>

      <span
        className="relative flex items-center justify-center overflow-hidden"
        style={{
          width: visorW,
          height: visorH,
          borderRadius: Math.round(visorH * 0.52),
          background: 'linear-gradient(180deg, #101014 0%, #030305 100%)',
          boxShadow: 'inset 0 1.5px 3px rgba(0,0,0,0.95), 0 1px 1px rgba(255,255,255,0.18)',
          border: '1px solid rgba(255,255,255,0.14)',
        }}
      >
        {/* The gloss across the top of the visor. Without it the bead reads as a sticker. */}
        <span
          className="pointer-events-none absolute inset-x-0 top-0 h-[40%] rounded-t-full"
          style={{
            background:
              'linear-gradient(180deg, rgba(255,255,255,0.16) 0%, rgba(255,255,255,0.01) 100%)',
          }}
        />
        {(['idle', 'working'] as const).map((mode) => (
          <span
            key={mode}
            className={`wgm-avatar-eyes wgm-avatar-eyes-${mode} absolute inset-0 z-10 flex items-center justify-center`}
            style={{ gap: eyeGap, ...(mode === 'idle' ? idle : busy) }}
          >
            {[0, 1].map((eye) => (
              <span
                key={eye}
                className="block"
                style={{
                  width: eyeW,
                  height: eyeH,
                  borderRadius: eyeRadius,
                  backgroundColor: '#ffffff',
                  boxShadow: `0 0 4px #ffffff, 0 0 8px #ffffff, 0 0 14px ${shift(color, 20)}`,
                }}
              />
            ))}
          </span>
        ))}
      </span>
    </span>
  )
}

/**
 * The wordmark: two bars in a disc, and the name.
 *
 * Rakazo's shape, set in Geist rather than their Aeonik — Aeonik is licensed and cannot ship
 * in an open-source repository, and a wordmark that renders in whatever the browser happens
 * to have is worse than one set in the face the rest of the console uses.
 */
export function Wordmark() {
  return (
    <span className="flex items-center gap-2.5">
      <span className="flex h-9 w-9 items-center justify-center gap-1 rounded-full bg-card">
        <span className="h-3.5 w-[5px] rounded-full bg-primary" />
        <span className="h-3.5 w-[5px] rounded-full bg-primary" />
      </span>
      <span className="text-[1.3125rem] font-medium tracking-tight text-foreground">Wingman</span>
    </span>
  )
}
