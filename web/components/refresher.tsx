'use client'

// Keeping a server-rendered page fresh without moving its reads into the browser.
//
// Every panel on this console is read on the server, because that is where the credentials
// are: the goal-engine bot key is in this process's environment and never leaves it. The
// alternative — a client poll per panel — would mean a proxy route and a table row for each
// one, and the browser holding a copy of the join the server already did.
//
// So instead: router.refresh() on an interval. Next re-runs the server components for the
// current route and patches the result in. Client state survives it, which matters because
// the composer may have half a sentence in it and the reason field may be open.
//
// Two behaviours worth naming:
//
//   A hidden tab does not refresh. An ops console left open on another desktop should not
//   be a load generator against two services, and it re-reads when it becomes visible.
//   The interval is the instance's own WEB_POLL_INTERVAL_MS, passed down from the server
//   rather than read here, because config() holds keys and never reaches a client bundle.

import { useEffect } from 'react'
import { useRouter } from 'next/navigation'

type Props = {
  intervalMs: number
}

export function Refresher({ intervalMs }: Props) {
  const router = useRouter()

  useEffect(() => {
    const tick = () => {
      if (!document.hidden) router.refresh()
    }
    const timer = setInterval(tick, intervalMs)
    document.addEventListener('visibilitychange', tick)
    return () => {
      clearInterval(timer)
      document.removeEventListener('visibilitychange', tick)
    }
  }, [intervalMs, router])

  return null
}
