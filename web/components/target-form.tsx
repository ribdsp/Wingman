'use client'

// Moving a goal's target, and pausing it.
//
// Both are operator-only in the engine, checked on the route and again in the service, and the
// reason is in the engine's own comment: lowering a target and meeting it are indistinguishable
// afterwards. So this form is the console's one place where the bar itself can move, it asks
// for confirmation naming both numbers, and every change lands in the audit log with the
// operator's key against it.
//
// Two statuses are offered and three are not. active and paused are lifecycle — an operator
// deciding whether the engine should keep chasing this. achieved, missed and archived are the
// evaluator's verdicts, and a console that could write them would let a person settle a goal
// without meeting it.

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import Link from 'next/link'
import { buttonClass, INPUT } from './chrome'
import { call, message } from '@/lib/client'
import { count } from '@/lib/format'
import type { Goal } from '@/lib/types'

/** What is being asked for, held between the click and the confirmation. */
type Pending =
  | { kind: 'target'; value: number }
  | { kind: 'status'; value: 'active' | 'paused' }

type Props = {
  goal: Goal
  isOperator: boolean
}

export function TargetForm({ goal, isOperator }: Props) {
  const router = useRouter()
  const [draft, setDraft] = useState(String(goal.targetValue))
  const [pending, setPending] = useState<Pending | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  if (!isOperator) {
    return (
      <p className="text-table text-faint">
        Moving a target or pausing a goal needs an operator key.{' '}
        <Link href="/authority" className="text-muted-foreground underline decoration-input">
          Hold one
        </Link>
        .
      </p>
    )
  }

  const isLifecycle = goal.status === 'active' || goal.status === 'paused'

  const ask = () => {
    setError('')
    const value = Number(draft.trim())
    if (!Number.isFinite(value)) {
      setError('A target is a number.')
      return
    }
    if (value === goal.targetValue) {
      setError('That is the current target.')
      return
    }
    setPending({ kind: 'target', value })
  }

  const commit = async () => {
    if (pending === null || busy) return
    setBusy(true)
    setError('')
    try {
      const body =
        pending.kind === 'target' ? { targetValue: pending.value } : { status: pending.value }
      await call(`/api/engine/v1/goals/${goal.id}`, 'PATCH', body)
      setPending(null)
      router.refresh()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <form
        onSubmit={(event) => {
          event.preventDefault()
          ask()
        }}
        className="flex items-end gap-2"
      >
        <span className="flex w-40 flex-col gap-0.5">
          <label htmlFor="target" className="label">
            target
          </label>
          <input
            id="target"
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            inputMode="decimal"
            autoComplete="off"
            spellCheck={false}
            className={`${INPUT} num`}
          />
        </span>
        <button type="submit" className={buttonClass('act')}>
          Move target
        </button>
        {isLifecycle && (
          <button
            type="button"
            onClick={() =>
              setPending({ kind: 'status', value: goal.status === 'active' ? 'paused' : 'active' })
            }
            className={buttonClass()}
          >
            {goal.status === 'active' ? 'Pause' : 'Resume'}
          </button>
        )}
      </form>

      {pending !== null && (
        <div className="flex items-center gap-2 border border-warning-dim px-2 py-1">
          <span className="text-table text-foreground">
            {pending.kind === 'target' ? (
              <>
                Move the target from <span className="num">{count(goal.targetValue)}</span> to{' '}
                <span className="num text-warning">{count(pending.value)}</span>? Recorded against
                your operator key.
              </>
            ) : pending.value === 'paused' ? (
              <>Pause this goal? The monitor stops evaluating it and will not trigger on it.</>
            ) : (
              <>Resume this goal? The monitor starts evaluating it again on the next tick.</>
            )}
          </span>
          <span className="ml-auto flex gap-2">
            <button
              type="button"
              onClick={() => void commit()}
              disabled={busy}
              className={buttonClass('act')}
            >
              {busy ? 'Saving' : 'Confirm'}
            </button>
            <button
              type="button"
              onClick={() => setPending(null)}
              disabled={busy}
              className={buttonClass()}
            >
              Cancel
            </button>
          </span>
        </div>
      )}

      {error !== '' && (
        <p role="alert" className="text-table text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}
