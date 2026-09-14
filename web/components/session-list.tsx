'use client'

// Where this account is signed in.
//
// Sessions are opaque tokens stored hashed in core, so this is the only place a person can see
// them, and the reason it exists is the reason core hashes them: a session you cannot see is a
// session you cannot notice is not yours.
//
// Core does not say which row is the one you are reading this on — the token lives in an
// httpOnly cookie and its id never reaches the page — so revoking is not offered with a
// promise about which browser survives. The page above says so plainly instead.

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { buttonClass, Empty, Id, TABLE, TD, TD_WIDE, TH } from './chrome'
import { call, message } from '@/lib/client'
import { short, stamp } from '@/lib/format'
import type { Session } from '@/lib/types'

export function SessionList({ items }: { items: readonly Session[] }) {
  const router = useRouter()
  const [asked, setAsked] = useState('')
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')

  if (items.length === 0) {
    return <Empty>No sessions. Not even this one, which means core did not answer.</Empty>
  }

  const revoke = async (id: string) => {
    if (busy !== '') return
    setBusy(id)
    setError('')
    try {
      await call(`/api/core/v1/me/sessions/${id}`, 'DELETE')
      setAsked('')
      router.refresh()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy('')
    }
  }

  return (
    <div className="flex flex-col gap-1">
      <table className={TABLE}>
        <caption className="sr-only">Sessions on this account, newest first</caption>
        <thead>
          <tr>
            <th className={TH} scope="col">
              started
            </th>
            <th className={TH} scope="col">
              last seen
            </th>
            <th className={TH} scope="col">
              expires
            </th>
            <th className={TH} scope="col">
              from
            </th>
            <th className={TH} scope="col">
              agent
            </th>
            <th className={TH} scope="col">
              state
            </th>
            <th className={TH} scope="col">
              <span className="sr-only">revoke</span>
            </th>
          </tr>
        </thead>
        <tbody>
          {items.map((session) => (
            <tr key={session.id}>
              <td className={`${TD} text-muted-foreground`}>{stamp(session.createdAt, true)}</td>
              <td className={`${TD} text-faint`}>
                {session.lastSeenAt === null ? '—' : stamp(session.lastSeenAt, true)}
              </td>
              <td className={`${TD} text-faint`}>{stamp(session.expiresAt)}</td>
              <td className={TD}>
                <Id>{session.createdIp}</Id>
              </td>
              {/* Truncated, not hidden: a user agent is how you recognise a browser you do not
                  remember signing in from, and it is also long enough to break this table. */}
              <td className={TD_WIDE}>
                <span className="text-faint">{short(session.userAgent, 40)}</span>
              </td>
              <td className={TD}>
                {session.isLive ? (
                  <span className="text-success">live</span>
                ) : session.revokedAt !== null ? (
                  <span className="text-faint">revoked</span>
                ) : (
                  <span className="text-faint">expired</span>
                )}
              </td>
              <td className={TD}>
                {!session.isLive ? null : asked === session.id ? (
                  <span className="flex items-center gap-2">
                    <button
                      type="button"
                      onClick={() => void revoke(session.id)}
                      disabled={busy !== ''}
                      className={buttonClass('stop')}
                    >
                      {busy === session.id ? 'Revoking' : 'Confirm'}
                    </button>
                    <button
                      type="button"
                      onClick={() => setAsked('')}
                      disabled={busy !== ''}
                      className={buttonClass()}
                    >
                      Keep
                    </button>
                  </span>
                ) : (
                  <button
                    type="button"
                    onClick={() => setAsked(session.id)}
                    className={buttonClass()}
                  >
                    Revoke
                  </button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      {error !== '' && (
        <p role="alert" className="text-table text-destructive">
          {error}
        </p>
      )}
    </div>
  )
}
