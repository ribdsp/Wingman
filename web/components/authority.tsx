'use client'

// Which credential an action is about to use, and the form that holds one.
//
// The console has three credentials and they are not interchangeable: the signed-in person's
// core session, a goal-engine bot key in this server's environment, and — only when somebody
// pastes it — a goal-engine operator key. Two operations need the third one: resolving a
// spend request and editing a goal's target. Both are gated on the engine's route *and* in
// its service, so a console that hid the distinction would just produce 403s that look like
// outages.
//
// So the chip is always on screen, naming what is held and for how long. The key itself is
// never here: it is posted once, verified by a side-effect-free probe, and sealed into an
// httpOnly cookie the browser cannot read. What comes back is "held" and an expiry.
//
// There is no renewal on activity. Authority to move the bar an agent is measured against, or
// to approve a spend, lapses on a clock and is taken again deliberately.

import { useState } from 'react'
import { buttonClass, INPUT } from './chrome'
import { call, message } from '@/lib/client'
import { remaining } from '@/lib/format'

/** held reports whether an expiry is still in the future, given the tick the caller has. */
export function isHeld(expiresAt: number | null, now: number): boolean {
  return expiresAt !== null && remaining(expiresAt, now) !== null
}

type ChipProps = {
  expiresAt: number | null
  now: number
}

/**
 * AuthorityChip names the credential in one line.
 *
 * The attention colour while operator authority is held, because that is a state worth
 * noticing — this browser can currently resolve spend requests — and quiet the rest of the
 * time.
 */
export function AuthorityChip({ expiresAt, now }: ChipProps) {
  const left = expiresAt === null ? null : remaining(expiresAt, now)
  if (left === null) {
    return <span className="text-table text-faint">bot key</span>
  }
  return (
    <span className="text-table text-warning">
      operator key · <span className="num">{left}</span> left
    </span>
  )
}

type FormProps = {
  expiresAt: number | null
  now: number
  /** Re-read /api/operator, so the strip and the buttons agree. */
  onChanged: () => void
}

/** OperatorForm takes or drops operator authority for this browser. */
export function OperatorForm({ expiresAt, now, onChanged }: FormProps) {
  const [key, setKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const left = expiresAt === null ? null : remaining(expiresAt, now)

  const hold = async () => {
    if (key.trim() === '') {
      setError('Paste an operator key.')
      return
    }
    setBusy(true)
    setError('')
    try {
      await call('/api/operator', 'POST', { key: key.trim() })
      // Cleared on success and on failure alike: a key that the engine refused is not one
      // worth leaving in a field for somebody to walk past and read.
      setKey('')
      onChanged()
    } catch (thrown) {
      setKey('')
      setError(message(thrown))
    } finally {
      setBusy(false)
    }
  }

  const drop = async () => {
    setBusy(true)
    setError('')
    try {
      await call('/api/operator', 'DELETE')
      onChanged()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy(false)
    }
  }

  if (left !== null) {
    return (
      <div className="flex flex-col gap-1">
        <p className="text-body text-muted-foreground">
          Operator authority is held by this browser and lapses in <span className="num">{left}</span>.
          It is not renewed by activity.
        </p>
        <div>
          <button type="button" onClick={() => void drop()} disabled={busy} className={buttonClass()}>
            {busy ? 'Dropping' : 'Drop it now'}
          </button>
        </div>
        {error !== '' && (
          <p role="alert" className="text-table text-destructive">
            {error}
          </p>
        )}
      </div>
    )
  }

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault()
        void hold()
      }}
      className="flex flex-col gap-1"
    >
      <label htmlFor="operator-key" className="label">
        Operator key
      </label>
      <div className="flex items-center gap-2">
        <input
          id="operator-key"
          type="password"
          autoComplete="off"
          spellCheck={false}
          value={key}
          onChange={(event) => setKey(event.target.value)}
          placeholder="name:secret, as listed in GOAL_ENGINE_API_KEYS"
          className={`${INPUT} max-w-120`}
        />
        <button type="submit" disabled={busy} className={buttonClass('act')}>
          {busy ? 'Verifying' : 'Hold'}
        </button>
      </div>
      <p className="text-table text-faint">
        Verified against the engine before it is held, and sealed into a cookie this page cannot
        read. Needed to resolve a spend request, to release the kill switch, and to edit a goal
        target.
      </p>
      {error !== '' && (
        <p role="alert" className="text-table text-destructive">
          {error}
        </p>
      )}
    </form>
  )
}
