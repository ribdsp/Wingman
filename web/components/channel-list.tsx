'use client'

// The chat accounts that may speak for this account.
//
// This list is the second half of a rule that lives in core: an external id is attached to an
// account *only* by sending a single-use code over the channel, because the message is the
// proof. So there is nothing here that connects a chat — only a button that mints the code and
// shows it once, and a button that disconnects one.
//
// The code is rendered once and never again. Core stores a hash and has no route that reads one
// back, so a person who loses it mints another. Copying it out of the page is the only way it
// leaves, which is why it is large, monospaced and selectable rather than tucked into a toast.
//
// Unlinking matters more than it looks: while a chat is connected, whoever holds that chat
// account can spend this account's budget. That sentence is on screen, not in a doc.

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { buttonClass, Empty, Id, TABLE, TD, TH } from './chrome'
import { call, message } from '@/lib/client'
import { stamp } from '@/lib/format'
import type { ChannelIdentity, LinkCode } from '@/lib/types'

export function ChannelList({ items }: { items: readonly ChannelIdentity[] }) {
  const router = useRouter()
  const [minted, setMinted] = useState<LinkCode | null>(null)
  const [asked, setAsked] = useState('')
  const [busy, setBusy] = useState('')
  const [error, setError] = useState('')

  const mint = async () => {
    if (busy !== '') return
    setBusy('mint')
    setError('')
    try {
      const parsed = await call<LinkCode>('/api/core/v1/channels/link-codes', 'POST')
      const code = parsed.data
      // Guarded rather than trusted: this is the one value in the console that cannot be
      // fetched again, so rendering an empty box would send someone to mint a second code
      // without saying why the first one looked blank.
      if (code === null || code === undefined || typeof code.code !== 'string' || code.code === '') {
        setError('Core answered without a code. Nothing was connected; ask for another.')
        return
      }
      setMinted(code)
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy('')
    }
  }

  const unlink = async (id: string) => {
    if (busy !== '') return
    setBusy(id)
    setError('')
    try {
      await call(`/api/core/v1/channels/${id}`, 'DELETE')
      setAsked('')
      router.refresh()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy('')
    }
  }

  return (
    <div className="flex flex-col gap-3">
      <div className="flex items-center gap-3">
        <button
          type="button"
          onClick={() => void mint()}
          disabled={busy !== ''}
          className={buttonClass('act')}
        >
          {busy === 'mint' ? 'Minting' : 'Get a link code'}
        </button>
        <span className="text-table text-faint">
          Send the code to the bot from the chat account you want to connect. It works once and
          expires in minutes.
        </span>
      </div>

      {minted !== null && (
        <div className="flex flex-col gap-1 border border-warning-dim px-3 py-2">
          <span className="label text-warning">shown once</span>
          <code className="num text-instrument leading-none tracking-widest text-foreground select-all">
            {minted.code}
          </code>
          <span className="text-table text-muted-foreground">
            Expires {stamp(minted.expiresAt, true)}. Core keeps only a hash of it — if it is lost,
            mint another.
          </span>
          <button
            type="button"
            onClick={() => setMinted(null)}
            className={`${buttonClass()} self-start`}
          >
            Hide
          </button>
        </div>
      )}

      {items.length === 0 ? (
        <Empty>No chat account is connected. Nothing can reach this account over a channel.</Empty>
      ) : (
        <table className={TABLE}>
          <caption className="sr-only">Connected chat accounts</caption>
          <thead>
            <tr>
              <th className={TH} scope="col">
                platform
              </th>
              <th className={TH} scope="col">
                name
              </th>
              <th className={TH} scope="col">
                external id
              </th>
              <th className={TH} scope="col">
                connected
              </th>
              <th className={TH} scope="col">
                state
              </th>
              <th className={TH} scope="col">
                <span className="sr-only">disconnect</span>
              </th>
            </tr>
          </thead>
          <tbody>
            {items.map((identity) => (
              <tr key={identity.id}>
                <td className={`${TD} text-foreground`}>{identity.kind}</td>
                <td className={`${TD} text-muted-foreground`}>
                  {identity.displayName === '' ? '—' : identity.displayName}
                </td>
                <td className={TD}>
                  <Id>{identity.externalId}</Id>
                </td>
                <td className={`${TD} text-faint`}>{stamp(identity.linkedAt)}</td>
                <td className={TD}>
                  {identity.isLive ? (
                    <span className="text-success">live</span>
                  ) : (
                    <span className="text-faint">
                      {identity.revokedAt === null ? 'inactive' : 'disconnected'}
                    </span>
                  )}
                </td>
                <td className={TD}>
                  {!identity.isLive ? null : asked === identity.id ? (
                    <span className="flex items-center gap-2">
                      <button
                        type="button"
                        onClick={() => void unlink(identity.id)}
                        disabled={busy !== ''}
                        className={buttonClass('stop')}
                      >
                        {busy === identity.id ? 'Disconnecting' : 'Confirm'}
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
                      onClick={() => setAsked(identity.id)}
                      className={buttonClass()}
                    >
                      Disconnect
                    </button>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {asked !== '' && (
        <p className="text-table text-muted-foreground">
          Disconnecting stops that chat account from being acted on. Until then, anyone who can
          type in it spends this account&apos;s budget.
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
