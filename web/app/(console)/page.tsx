// The deck: the first screen, and the only one an operator should need open.
//
// The order is fixed by docs/roadmap.md item 3 and is not a layout preference — it is what
// gets read first during an incident: who is waiting on a person, then whether anything is
// behind, then what the monitor just decided, then what has happened, then a way to say
// something. Read-mostly. Two things on this screen change the world: resolving an approval,
// and the kill switch in the strip above.
//
// Every panel is read on the server and every read is wrapped, so a panel that failed says so
// in rust under its own heading and the other four still work. A deck that 500s because the
// audit query is slow tells an operator nothing during the exact minute they opened it.
//
// One <Refresher> for the page rather than a poll per panel: the credentials are in this
// process's environment, so the reads stay here and router.refresh() re-runs them.

import Link from 'next/link'
import { ApprovalQueue } from '@/components/approval-queue'
import { AuditTable } from '@/components/audit-table'
import { Chat } from '@/components/chat'
import { Panel } from '@/components/chrome'
import { PaceTable, TickTable } from '@/components/pace-table'
import { Refresher } from '@/components/refresher'
import { readApprovals, readAudit, readBoard, readDeckChat } from '@/lib/deck'
import { config } from '@/lib/env'
import { count, stamp } from '@/lib/format'
import { behindCount } from '@/lib/pace'
import { operatorExpiresAt } from '@/lib/session'

/** How much of each list the deck shows before it is a page rather than a glance. */
const QUEUE = 10
const EVENTS = 12
const TRANSCRIPT = 20

export default async function Deck() {
  const [held, approvals, board, log, thread] = await Promise.all([
    operatorExpiresAt(),
    readApprovals(QUEUE),
    readBoard(),
    readAudit('', EVENTS),
    readDeckChat(TRANSCRIPT),
  ])

  // Computed here rather than with isHeld from components/authority.tsx: that module is a
  // client component and a server component may not call into one. The same comparison,
  // against this request's clock.
  const isOperator = held !== null && held > Date.now()

  const waiting = approvals.value.total ?? approvals.value.items.length
  const behind = behindCount(board.value.rows)
  const newest = board.value.tick[0]
  const { pollIntervalMs } = config()

  return (
    <>
      <Refresher intervalMs={pollIntervalMs} />

      {/* First, because it is the only thing on this console that is blocked on a human. */}
      <Panel
        label="waiting on a person"
        count={waiting === 0 ? 'none' : count(waiting)}
        error={approvals.error}
      >
        <ApprovalQueue items={approvals.value.items} isOperator={isOperator} />
      </Panel>

      <Panel
        label="pace"
        count={behind === 0 ? 'all on pace' : <>{count(behind)} behind</>}
        error={board.error}
        action={
          <Link
            href="/goals"
            className="text-table text-muted-foreground underline decoration-input hover:text-foreground"
          >
            every goal
          </Link>
        }
      >
        <PaceTable rows={board.value.rows} />
      </Panel>

      <Panel
        label="last tick"
        count={board.value.tick.length === 0 ? undefined : count(board.value.tick.length)}
        action={
          newest === undefined ? undefined : (
            <span className="num text-table text-faint">{stamp(newest.evaluatedAt, true)}</span>
          )
        }
      >
        <TickTable tick={board.value.tick} />
      </Panel>

      {/* The filter and the pager live on /audit. A filter here would be one that cannot page
          past its first screen, which is a worse answer than a link to the page that can. */}
      <Panel
        label="audit"
        error={log.error}
        action={
          <Link
            href="/audit"
            className="text-table text-muted-foreground underline decoration-input hover:text-foreground"
          >
            filter and page
          </Link>
        }
      >
        <AuditTable items={log.value.items} />
      </Panel>

      <Panel
        label="chat"
        error={thread.error}
        action={
          <span className="flex items-center gap-3">
            {thread.value.chat !== null && (
              <span className="text-table text-faint">{thread.value.chat.title}</span>
            )}
            <Link
              href="/chat"
              className="text-table text-muted-foreground underline decoration-input hover:text-foreground"
            >
              every conversation
            </Link>
          </span>
        }
      >
        <Chat
          chatId={thread.value.chat?.id ?? null}
          initial={thread.value.messages}
          pollIntervalMs={pollIntervalMs}
          truncated={thread.value.truncated}
          size={TRANSCRIPT}
          height="max-h-[32vh]"
        />
      </Panel>
    </>
  )
}
