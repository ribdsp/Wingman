'use client'

// The approval queue: the one place in this console where money is decided.
//
// It is first on the deck because it is the only panel where something is waiting on a
// person. Everything else here reports; this asks.
//
// Three deliberate frictions, all of them the engine's semantics made visible:
//
//   Resolving needs an operator key, checked on the engine's route and again in its service.
//   Without one the buttons are not rendered at all and the row says which credential is
//   missing — a console that showed a button which answers 403 teaches an operator to
//   distrust the console rather than to go and hold a key.
//
//   A click expands a confirmation line naming the amount, rather than resolving. This is
//   the only button in the console that releases money; a misclick on a dense table should
//   not be able to do that. The note is optional, as it is in the API.
//
//   `pending` is the outcome the ladder reached, not a state the console invented. A queue
//   row exists because a policy said "ask a human", and the policy's own reason is shown
//   beside it so the decision is made against the rule that produced it.

import { useState } from 'react'
import type { ReactNode } from 'react'
import Link from 'next/link'
import { useRouter } from 'next/navigation'
import { buttonClass, Empty, Id, INPUT, Mark, TABLE, TD, TH, Word } from './chrome'
import { call, message, useNow } from '@/lib/client'
import { money, remaining, short, stamp } from '@/lib/format'
import { outcomeTone } from '@/lib/tone'
import type { Approval } from '@/lib/types'

type Resolution = 'approved' | 'rejected'

type Props = {
  items: readonly Approval[]
  /** Whether operator authority is held. Without it the engine refuses to resolve. */
  isOperator: boolean
}

export function ApprovalQueue({ items, isOperator }: Props) {
  const router = useRouter()
  const now = useNow()
  const [pending, setPending] = useState<{ id: string; resolution: Resolution } | null>(null)
  const [note, setNote] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  if (items.length === 0) {
    return <Empty>Nothing is waiting for a decision.</Empty>
  }

  const ask = (id: string, resolution: Resolution) => {
    setPending({ id, resolution })
    setNote('')
    setError('')
  }

  const resolve = async () => {
    if (!pending) return
    setBusy(true)
    setError('')
    try {
      await call(`/api/engine/v1/approvals/${pending.id}/resolve`, 'POST', {
        resolution: pending.resolution,
        note: note.trim(),
      })
      setPending(null)
      setNote('')
      // The queue is a server read. Refreshing re-runs it rather than editing a local copy,
      // so what is on screen is what the engine says and not what this browser assumed.
      router.refresh()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy(false)
    }
  }

  return (
    <table className={TABLE}>
      <caption className="sr-only">Spend requests waiting for a human decision</caption>
      <thead>
        <tr>
          <th className={TH} scope="col">
            action
          </th>
          <th className={`${TH} text-right`} scope="col">
            amount
          </th>
          <th className={TH} scope="col">
            requested by
          </th>
          <th className={TH} scope="col">
            goal
          </th>
          <th className={TH} scope="col">
            filed
          </th>
          <th className={TH} scope="col">
            expires
          </th>
          <th className={`${TH} w-px`} scope="col">
            <span className="sr-only">decide</span>
          </th>
        </tr>
      </thead>
      <tbody>
        {items.map((item) => {
          const expiry = item.expiresAt === null ? null : Date.parse(item.expiresAt)
          const left = expiry === null || Number.isNaN(expiry) ? null : remaining(expiry, now)
          // Kept as the object rather than a boolean so the confirmation line below can read
          // which way it is about to go without TypeScript having to be told twice.
          const asked = pending !== null && pending.id === item.id ? pending : null

          return (
            <Row key={item.id}>
              <tr>
                <td className={TD}>
                  <span className="flex items-center gap-1.5">
                    <Mark tone={outcomeTone(item.outcome)} />
                    <Word value={item.actionType} tone="quiet" />
                  </span>
                </td>
                <td className={`${TD} num text-right text-foreground`}>{money(item.amount, item.currency)}</td>
                <td className={TD}>
                  <span className="text-muted-foreground">{item.requestedBy}</span>
                </td>
                <td className={TD}>
                  {item.goalId === null ? (
                    <span className="text-faint">—</span>
                  ) : (
                    <Link href={`/goals/${item.goalId}`} className="hover:text-foreground">
                      <Id>{short(item.goalId)}</Id>
                    </Link>
                  )}
                </td>
                <td className={`${TD} text-faint`}>{stamp(item.createdAt)}</td>
                <td className={TD}>
                  {item.expiresAt === null ? (
                    <span className="text-faint">no expiry</span>
                  ) : left === null ? (
                    // Past its expiry and still open: the sweep that expires requests has
                    // not run yet. Worth seeing rather than hiding, because the engine will
                    // resolve it as expired and nobody will have decided.
                    <span className="text-destructive">lapsed</span>
                  ) : (
                    <span className="num text-warning">{left}</span>
                  )}
                </td>
                <td className={`${TD} text-right`}>
                  {!isOperator ? (
                    <span className="text-faint">
                      needs an{' '}
                      <Link
                        href="/authority"
                        className="text-muted-foreground underline decoration-input"
                      >
                        operator key
                      </Link>
                    </span>
                  ) : (
                    <span className="flex items-center justify-end gap-2">
                      <button
                        type="button"
                        onClick={() => ask(item.id, 'approved')}
                        className={buttonClass('act')}
                      >
                        Approve
                      </button>
                      <button
                        type="button"
                        onClick={() => ask(item.id, 'rejected')}
                        className={buttonClass('stop')}
                      >
                        Reject
                      </button>
                    </span>
                  )}
                </td>
              </tr>

              {item.policyReason !== undefined && item.policyReason !== '' && (
                <tr>
                  <td className="pb-1 text-faint" colSpan={7}>
                    {item.policyReason}
                  </td>
                </tr>
              )}

              {asked !== null && (
                <tr>
                  <td className="border-b border-border py-2" colSpan={7}>
                    <div className="crossfade flex flex-col gap-1">
                      <p className={asked.resolution === 'approved' ? 'text-warning' : 'text-destructive'}>
                        {asked.resolution === 'approved'
                          ? `Release ${money(item.amount, item.currency)} for ${item.actionType}?`
                          : `Refuse ${money(item.amount, item.currency)} for ${item.actionType}?`}
                      </p>
                      <div className="flex items-center gap-2">
                        <label htmlFor={`note-${item.id}`} className="label shrink-0">
                          note
                        </label>
                        <input
                          id={`note-${item.id}`}
                          autoFocus
                          value={note}
                          onChange={(event) => setNote(event.target.value)}
                          onKeyDown={(event) => {
                            if (event.key === 'Enter') void resolve()
                            if (event.key === 'Escape') setPending(null)
                          }}
                          placeholder="optional, kept in the audit log"
                          className={`${INPUT} max-w-120`}
                        />
                        <button
                          type="button"
                          onClick={() => void resolve()}
                          disabled={busy}
                          className={buttonClass(asked.resolution === 'approved' ? 'act' : 'stop')}
                        >
                          {busy ? 'Recording' : asked.resolution === 'approved' ? 'Approve' : 'Reject'}
                        </button>
                        <button
                          type="button"
                          onClick={() => setPending(null)}
                          disabled={busy}
                          className={buttonClass()}
                        >
                          Cancel
                        </button>
                      </div>
                      {error !== '' && (
                        <p role="alert" className="text-destructive">
                          {error}
                        </p>
                      )}
                    </div>
                  </td>
                </tr>
              )}
            </Row>
          )
        })}
      </tbody>
    </table>
  )
}

/**
 * Row groups a request's rows so the confirmation line belongs to the request above it.
 *
 * A fragment rather than a <tbody> per row: multiple bodies in one table are valid but read
 * as separate row groups to a screen reader, which this is not — it is one request with a
 * reason and, sometimes, a question.
 */
function Row({ children }: { children: ReactNode }) {
  return <>{children}</>
}
