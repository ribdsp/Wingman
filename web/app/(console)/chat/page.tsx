// Every conversation, and a composer that starts a new one.
//
// The deck's chat panel talks into the most recent conversation, which is the right default for
// a glance and the wrong one for anything else. This page is the other two things: pick an
// older thread, or start a thread that is not the last one.
//
// The list is live conversations only — core's chat list excludes archived ones unless asked,
// and it is not asked here. An archived conversation is one somebody put away.

import Link from 'next/link'
import { Chat } from '@/components/chat'
import { Empty, Id, Panel, TABLE, TD, TD_WIDE, TH } from '@/components/chrome'
import { Refresher } from '@/components/refresher'
import { readChats } from '@/lib/deck'
import { config } from '@/lib/env'
import { count, short, stamp } from '@/lib/format'

const PER_PAGE = 40

export default async function Chats() {
  const chats = await readChats(PER_PAGE)
  const items = chats.value
  const { pollIntervalMs } = config()

  return (
    <>
      <Refresher intervalMs={pollIntervalMs} />

      <Panel
        label="conversations"
        count={items.length === 0 ? undefined : count(items.length)}
        error={chats.error}
      >
        {items.length === 0 ? (
          <Empty>No conversations yet. The composer below starts one.</Empty>
        ) : (
          <table className={TABLE}>
            <caption className="sr-only">Conversations, most recently active first</caption>
            <thead>
              <tr>
                <th className={TH} scope="col">
                  conversation
                </th>
                <th className={TH} scope="col">
                  last activity
                </th>
                <th className={TH} scope="col">
                  started
                </th>
                <th className={TH} scope="col">
                  id
                </th>
              </tr>
            </thead>
            <tbody>
              {items.map((chat) => (
                <tr key={chat.id}>
                  <td className={TD_WIDE}>
                    <Link href={`/chat/${chat.id}`} className="text-foreground hover:underline">
                      {chat.title === '' ? 'untitled' : chat.title}
                    </Link>
                  </td>
                  <td className={`${TD} text-muted-foreground`}>{stamp(chat.updatedAt, true)}</td>
                  <td className={`${TD} text-faint`}>{stamp(chat.createdAt, true)}</td>
                  <td className={TD}>
                    <Id>{short(chat.id)}</Id>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      {/* chatId null is how core is told to open a new conversation: the first send carries no
          chat id and the response says which one it made. Same component as everywhere else —
          a second composer that only creates would be a second thing to keep correct. */}
      <Panel label="new conversation">
        <Chat chatId={null} initial={[]} pollIntervalMs={pollIntervalMs} height="max-h-[24vh]" />
      </Panel>
    </>
  )
}
