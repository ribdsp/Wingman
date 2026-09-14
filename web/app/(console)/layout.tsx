// The frame every console page hangs in, and the gate in front of it.
//
// Two things happen here and nowhere else: a request with no valid session is sent to the
// sign-in screen, and the status strip is filled in. Both belong in the layout because both
// are true of every page — a console where one screen forgot to check the kill switch would
// be a console with a screen you could act from while everything was halted.
//
// The route group exists for exactly this. /signin sits outside it, so the gate cannot lock
// somebody out of the door they came to open.

import type { ReactNode } from 'react'
import { redirect } from 'next/navigation'
import { DeckShell } from '@/components/deck-shell'
import { readIdentity, readShell } from '@/lib/deck'

// Nothing under here is cacheable. Every page reads a per-request cookie and two services
// whose answers change on their own — a cached deck is somebody else's queue, or last hour's.
export const dynamic = 'force-dynamic'

export default async function ConsoleLayout({ children }: { children: ReactNode }) {
  // Sequential on purpose, and the only place in this console that is. A request with no
  // session should cost one read and a redirect; reading the strip in parallel would mean
  // four upstream calls whose answers are thrown away on the way to /signin.
  const viewer = await readIdentity()
  // `stale` because a browser can arrive here holding a cookie that seals correctly and a
  // token core has since revoked or expired — which is exactly what /authority's own Revoke
  // button does to the row you are sitting on. Without the flag, /signin sees a well-sealed
  // cookie, sends the browser back to the deck, and the two redirects loop until the browser
  // gives up: ERR_TOO_MANY_REDIRECTS instead of the sign-in form, and an operator locked out
  // of their own instance with no way to guess that clearing a cookie is the escape.
  //
  // Only a dead session reaches this line. readIdentity returns null for a 401 or 403 and
  // falls back to the sealed claims when core simply does not answer, so a chat service that
  // is down never sends anybody to sign in again.
  if (viewer === null) redirect('/signin?stale=1')

  const { pollIntervalMs, ...strip } = await readShell()

  return (
    <DeckShell viewer={viewer} initial={strip} pollIntervalMs={pollIntervalMs}>
      {children}
    </DeckShell>
  )
}
