// One conversation, at length.
//
// The window is the tail, not the beginning: readThread pages to the end of a long thread, and
// the composer's own poll asks for the same window from the same end. Both use WINDOW, so what
// the server rendered and what the browser then re-reads are the same slice — if they differed,
// the first poll would silently redraw the transcript at a different length.
//
// No <Refresher> on this page. The composer already polls its own transcript, and the only other
// thing here is a title; re-running the server render under a half-typed message would be a
// worse trade than a header that is a minute old.

import { Empty, Id, Panel } from '@/components/chrome'
import { Chat } from '@/components/chat'
import { readThread } from '@/lib/deck'
import { config } from '@/lib/env'
import { stamp } from '@/lib/format'

/** Messages held on screen. Matches readThread's own default; stated once and shared. */
const WINDOW = 60

type Props = {
  params: Promise<{ id: string }>
}

export default async function Conversation({ params }: Props) {
  const { id } = await params
  const thread = await readThread(id, WINDOW)
  const { chat, messages, truncated } = thread.value
  const { pollIntervalMs } = config()

  // As on a goal's page: no notFound(). A conversation that does not exist and a core that did
  // not answer arrive here identically, and only one of them means the thread is gone.
  if (chat === null) {
    return (
      <Panel label="conversation" error={thread.error}>
        <Empty>
          Nothing came back for <Id>{id}</Id>. Either it is not yours, or core did not answer.
        </Empty>
      </Panel>
    )
  }

  return (
    <Panel
      label={chat.title === '' ? 'untitled' : chat.title}
      error={thread.error}
      action={
        <span className="flex items-center gap-3">
          <span className="num text-table text-faint">{stamp(chat.createdAt, true)}</span>
          <Id>{chat.id}</Id>
        </span>
      }
    >
      <Chat
        chatId={chat.id}
        initial={messages}
        pollIntervalMs={pollIntervalMs}
        truncated={truncated}
        size={WINDOW}
        height="max-h-[70vh]"
      />
    </Panel>
  )
}
