'use client'

// The kill switch. One button, and the only one on the deck that stops the world.
//
// Two asymmetries are the engine's, not this component's, and the UI states both rather
// than smoothing them over:
//
//   Engaging is open to anyone the engine will talk to — whoever notices the damage should
//   be able to stop it, and the worst case is an outage a human undoes in one call.
//   Releasing is an operator's alone, refused in the engine's service. So Release is
//   disabled here when no operator authority is held, and says why, instead of offering a
//   button that answers 403.
//
//   Unreadable reads as engaged. The engine's rule, and core's: unreadable means halt. The
//   panel shows both sentences at once — "engaged" and "the engine did not answer" — because
//   an operator at 2am needs to know which one they are looking at.
//
// A reason is required in both directions. The engine rejects an empty one, and the reason
// is the line somebody reads later while working out why the fleet went quiet.

import { useState } from 'react'
import Link from 'next/link'
import { buttonClass, INPUT } from './chrome'
import { call, message } from '@/lib/client'
import { stamp } from '@/lib/format'
import type { SwitchReading } from '@/lib/types'

type Props = {
  reading: SwitchReading
  /** Whether operator authority is currently held, which is what Release needs. */
  isOperator: boolean
  /** Re-read the switch after a change, so the strip stops disagreeing with the button. */
  onChanged: () => void
}

export function KillSwitch({ reading, isOperator, onChanged }: Props) {
  const [open, setOpen] = useState(false)
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const engaging = !reading.engaged

  const submit = async () => {
    const trimmed = reason.trim()
    if (trimmed === '') {
      setError('The engine requires a reason. It is what somebody reads later.')
      return
    }
    setBusy(true)
    setError('')
    try {
      await call('/api/engine/v1/flags/kill-switch', 'PUT', {
        engaged: engaging,
        reason: trimmed,
      })
      setOpen(false)
      setReason('')
      onChanged()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy(false)
    }
  }

  const close = () => {
    setOpen(false)
    setError('')
  }

  if (!open) {
    return (
      <div className="flex items-center gap-2">
        {engaging ? (
          <button type="button" onClick={() => setOpen(true)} className={buttonClass('stop')}>
            Engage kill switch
          </button>
        ) : isOperator ? (
          <button type="button" onClick={() => setOpen(true)} className={buttonClass('act')}>
            Release
          </button>
        ) : (
          <span className="text-table text-faint">
            Releasing needs an operator key ·{' '}
            <Link href="/authority" className="text-muted-foreground underline decoration-input">
              hold one
            </Link>
          </span>
        )}
        {reading.updatedAt !== null && (
          <span className="text-table text-faint">
            {reading.engaged ? 'engaged' : 'released'} {stamp(reading.updatedAt)}
            {reading.updatedBy === '' ? '' : ` by ${reading.updatedBy}`}
          </span>
        )}
      </div>
    )
  }

  return (
    <div className="flex flex-col gap-1">
      <div className="flex items-center gap-2">
        <label htmlFor="kill-reason" className="label shrink-0">
          {engaging ? 'Why stop' : 'Why release'}
        </label>
        <input
          id="kill-reason"
          autoFocus
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === 'Enter') void submit()
            if (event.key === 'Escape') close()
          }}
          placeholder={engaging ? 'agent is spending on the wrong goal' : 'cause found and fixed'}
          className={`${INPUT} max-w-96`}
        />
        <button
          type="button"
          onClick={() => void submit()}
          disabled={busy}
          className={buttonClass(engaging ? 'stop' : 'act')}
        >
          {busy ? 'Working' : engaging ? 'Stop everything' : 'Release'}
        </button>
        <button type="button" onClick={close} disabled={busy} className={buttonClass()}>
          Cancel
        </button>
      </div>
      {engaging && (
        <p className="text-table text-faint">
          Every trigger and every spend stops, on both services, until an operator releases it.
        </p>
      )}
      {error !== '' && (
        <p role="alert" className="text-table text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}
