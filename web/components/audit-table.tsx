// The audit log, as a table.
//
// The engine's audit log has no update and no delete method — not "admin only", none — and
// this is the console's only view of it. So the table's job is to be readable at length: an
// operator arrives here after something happened and reads downward until they find it.
//
// Two choices worth stating:
//
//   The outcome carries the only colour, and mostly it carries none. Most audit rows are
//   routine — created, updated, recorded, sent — and colouring all of them would leave a
//   screen where nothing stands out. See auditTone.
//
//   `detail` is flattened onto the row rather than expanded under it. The rows are the point;
//   a row that grew to eight lines would push the next event off the screen.
//
// Presentational: no state, no directive. The filter above it is the client component.

import type { ReactNode } from 'react'
import { Empty, Id, Mark, TABLE, TD, TD_WIDE, TH, Word } from './chrome'
import { clock, dayStamp, detailLine, short, stamp } from '@/lib/format'
import { auditTone } from '@/lib/tone'
import type { AuditEvent } from '@/lib/types'

const COLUMNS = 6

type Props = {
  items: readonly AuditEvent[]
  /** Break the rows under a date heading. On for the log page, off for the deck panel. */
  groupByDay?: boolean
}

export function AuditTable({ items, groupByDay = false }: Props) {
  if (items.length === 0) {
    return <Empty>No events match. The log itself is never empty — a filter is.</Empty>
  }

  return (
    <table className={TABLE}>
      <caption className="sr-only">Audit events, most recent first</caption>
      <thead>
        <tr>
          <th className={TH} scope="col">
            at
          </th>
          <th className={TH} scope="col">
            actor
          </th>
          <th className={TH} scope="col">
            action
          </th>
          <th className={TH} scope="col">
            subject
          </th>
          <th className={TH} scope="col">
            outcome
          </th>
          <th className={TH} scope="col">
            detail
          </th>
        </tr>
      </thead>
      <tbody>
        {items.map((event, index) => {
          const previous = index === 0 ? null : (items[index - 1] ?? null)
          const day = dayStamp(event.at)
          const isNewDay = groupByDay && (previous === null || dayStamp(previous.at) !== day)

          return (
            <Rows key={event.id}>
              {isNewDay && (
                <tr>
                  {/* text-left because a <th> is centred by default and this heading has to
                      line up with the timestamps under it, not float over the middle of the
                      table. TH carries the same class for the same reason. */}
                  <th
                    className="label h-row border-b border-border pt-2 text-left align-bottom"
                    colSpan={COLUMNS}
                    scope="rowgroup"
                  >
                    {day}
                  </th>
                </tr>
              )}
              <tr>
                {/* Time alone when a heading above already carries the date, which is the log
                    page. The deck panel is not grouped, so there the date has to be on the
                    row or a night's rows read as this morning's. */}
                <td className={`${TD} text-faint`}>
                  {groupByDay ? clock(event.at) : stamp(event.at, true)}
                </td>
                <td className={TD}>
                  <span className="text-muted-foreground">{event.actorType}</span>{' '}
                  <Id>{short(event.actorId, 12)}</Id>
                </td>
                <td className={`${TD} text-foreground`}>{event.action}</td>
                <td className={TD}>
                  <span className="text-muted-foreground">{event.subjectType}</span>{' '}
                  <Id>{short(event.subjectId)}</Id>
                </td>
                <td className={TD}>
                  <span className="flex items-center gap-1.5">
                    <Mark tone={auditTone(event.outcome)} />
                    <Word value={event.outcome} tone={auditTone(event.outcome)} />
                  </span>
                </td>
                {/* The request id sits with the detail rather than in a column of its own:
                    it is what you carry to the service's access log when the detail is not
                    enough, and it is not what anybody scans by. Id keeps it on one line —
                    see chrome.tsx — which is what holds this column open when the window
                    is narrow; the detail beside it is still free to wrap. */}
                <td className={TD_WIDE}>
                  <span className="text-muted-foreground">{detailLine(event.detail)}</span>
                  {event.requestId !== undefined && event.requestId !== '' && (
                    <>
                      {' '}
                      <Id>{event.requestId}</Id>
                    </>
                  )}
                </td>
              </tr>
            </Rows>
          )
        })}
      </tbody>
    </table>
  )
}

/**
 * Rows groups a date heading with the event under it.
 *
 * A fragment, for the same reason the approval queue uses one: a <tbody> per event would read
 * as a row group per event to a screen reader, and these are one list.
 */
function Rows({ children }: { children: ReactNode }) {
  return <>{children}</>
}
