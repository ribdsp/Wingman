'use client'

// Stopping a run that is still going.
//
// The kill switch stops everything; this stops one thing, and both exist for the same reason —
// whoever notices should be able to stop it. Core marks the run cancelled and the loop ends at
// its next boundary, so the button says "asked to stop" rather than pretending it happened at
// the moment of the click.
//
// Nothing is rendered for a finished run. A disabled Cancel next to a completed run is an
// invitation to wonder whether it would have worked.

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { buttonClass } from './chrome'
import { call, message } from '@/lib/client'

type Props = {
  runId: string
  isInFlight: boolean
  isCancelled: boolean
}

export function CancelRun({ runId, isInFlight, isCancelled }: Props) {
  const router = useRouter()
  const [asked, setAsked] = useState(false)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  if (!isInFlight) return null

  if (isCancelled) {
    return <span className="text-table text-faint">cancellation asked for</span>
  }

  const cancel = async () => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await call(`/api/core/v1/runs/${runId}/cancel`, 'POST')
      setAsked(false)
      router.refresh()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy(false)
    }
  }

  return (
    <span className="flex items-center gap-2">
      {asked ? (
        <>
          <span className="text-table text-foreground">Stop this run at its next step?</span>
          <button
            type="button"
            onClick={() => void cancel()}
            disabled={busy}
            className={buttonClass('stop')}
          >
            {busy ? 'Stopping' : 'Confirm'}
          </button>
          <button
            type="button"
            onClick={() => setAsked(false)}
            disabled={busy}
            className={buttonClass()}
          >
            Keep going
          </button>
        </>
      ) : (
        <button type="button" onClick={() => setAsked(true)} className={buttonClass('stop')}>
          Cancel run
        </button>
      )}
      {error !== '' && (
        <span role="alert" className="text-table text-destructive">
          {error}
        </span>
      )}
    </span>
  )
}
