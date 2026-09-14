// Who you are, what you are holding, and everywhere else that can act as you.
//
// One page for three kinds of authority, because they answer one question — who can currently
// do things as me — and it is the question asked after something unexpected happened:
//
//   the account itself,
//   operator authority, which is held for minutes and is never in this server's environment,
//   the sessions and the linked chats, either of which is somebody else with your budget.
//
// Three reads, three panels. A channel list that failed must not hide a session you do not
// recognise; both are places access could be sitting.

import { ChannelList } from '@/components/channel-list'
import { Empty, Field, Id, Num, Panel, Word } from '@/components/chrome'
import { OperatorPanel } from '@/components/operator-panel'
import { SessionList } from '@/components/session-list'
import { readAccount } from '@/lib/detail'
import { stamp } from '@/lib/format'
import { operatorExpiresAt } from '@/lib/session'

export default async function Authority() {
  const [held, account] = await Promise.all([operatorExpiresAt(), readAccount()])
  const me = account.me.value

  return (
    <>
      <Panel label="account" error={account.me.error}>
        {me === null ? (
          <Empty>Core did not answer for this account. The session cookie is still sealed.</Empty>
        ) : (
          <dl className="flex flex-col">
            <Field label="name">
              <span className="text-foreground">{me.displayName}</span>
            </Field>
            <Field label="email">
              <Num>{me.email}</Num>
            </Field>
            <Field label="status">
              <Word value={me.isActive ? 'active' : 'disabled'} tone={me.isActive ? 'quiet' : 'stop'} />
            </Field>
            <Field label="since">
              <Num>{stamp(me.createdAt, true)}</Num>
            </Field>
            <Field label="id">
              <Id>{me.id}</Id>
            </Field>
          </dl>
        )}
      </Panel>

      <Panel label="operator authority">
        {/* Said on the page, not only in the docs: an operator who does not know the key is not
            stored has no reason to expect it to lapse, and will read the lapse as a bug. */}
        <p className="max-w-[70ch] pb-3 text-table text-muted-foreground">
          The operator key is not in this server&apos;s environment and is never written to disk.
          Pasting it here checks it against the goal engine with a call that changes nothing, then
          seals it in a cookie for a few minutes. It is what releasing the kill switch and editing a
          goal&apos;s target require — engaging the switch requires nothing, deliberately, because
          anybody who notices should be able to stop the instance.
        </p>
        <OperatorPanel expiresAt={held} />
      </Panel>

      <Panel label="sessions" error={account.sessions.error}>
        {/* Core does not say which row is this browser, and this console does not guess. The
            session token it holds is stored hashed on the other side, so there is nothing to
            compare — revoking the wrong row signs you out, which is recoverable, and pretending
            to know which is which would not be. */}
        <p className="max-w-[70ch] pb-3 text-table text-muted-foreground">
          Every live session can act as you. Core cannot tell you which row is this browser —
          session tokens are stored hashed, so there is nothing here to match against — and
          revoking the one you are using signs you out.
        </p>
        <SessionList items={account.sessions.value} />
      </Panel>

      <Panel label="linked chats" error={account.channels.error}>
        <p className="max-w-[70ch] pb-3 text-table text-muted-foreground">
          A linked Telegram, Slack or Discord identity can spend this account&apos;s daily budget by
          typing. Linking happens one way only: a single-use code, shown once here and sent over the
          channel itself, so the message is the proof. Revoking a link stops that chat at the next
          message it sends.
        </p>
        <ChannelList items={account.channels.value} />
      </Panel>
    </>
  )
}
