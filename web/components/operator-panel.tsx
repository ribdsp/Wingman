'use client'

// The authority panel: the operator-key form, wired to a clock and a refresh.
//
// A thin wrapper, and it exists for one structural reason: the page that reads the account is a
// server component, and OperatorForm needs a ticking `now` for its countdown and a callback for
// when what is held changes. Neither survives the boundary, so the boundary is here rather than
// on the page.

import { useRouter } from 'next/navigation'
import { OperatorForm } from './authority'
import { useNow } from '@/lib/client'

export function OperatorPanel({ expiresAt }: { expiresAt: number | null }) {
  const router = useRouter()
  const now = useNow()

  // refresh, not a local flag: the status strip in the layout reads the same cookie, and two
  // places disagreeing about whether operator authority is held is the one thing this chip
  // exists to prevent.
  return <OperatorForm expiresAt={expiresAt} now={now} onChanged={() => router.refresh()} />
}
