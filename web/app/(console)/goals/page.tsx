// Every goal, not just the ones being chased.
//
// The deck's pace panel shows active goals, because those are the ones an agent can be woken
// for. This page exists for the other question — "why is nothing happening on that one" — and
// the answer is usually the status column: paused by somebody months ago, or missed when its
// period ended quietly. So the filter defaults to everything and the status is on screen.
//
// The filter and the position live in the URL, so a link to "the paused ones" is a link
// somebody else can open and see the same thing.

import Link from 'next/link'
import { Panel } from '@/components/chrome'
import { PaceTable } from '@/components/pace-table'
import { Pager } from '@/components/pager'
import { Refresher } from '@/components/refresher'
import { readGoalBoard } from '@/lib/detail'
import { config } from '@/lib/env'
import { count } from '@/lib/format'
import { behindCount } from '@/lib/pace'
import { firstValues, href, queryString, windowOf, withValue } from '@/lib/search'

/**
 * The engine's five goal statuses, in lifecycle order.
 *
 * The engine's own vocabulary, spelled as it spells it — these are the strings it accepts on
 * ?status= and writes into the audit log. An empty value is "no filter" rather than a sixth
 * status, which is why it is not in this list.
 */
const STATUSES: readonly string[] = ['active', 'paused', 'achieved', 'missed', 'archived']

/** Goals per page. Dense, and matched to the evaluations read so the join stays complete. */
const PER_PAGE = 50

type Props = {
  searchParams: Promise<Record<string, string | string[] | undefined>>
}

export default async function Goals({ searchParams }: Props) {
  const values = firstValues(await searchParams)
  const search = queryString(values)
  const { limit, offset } = windowOf(values, PER_PAGE)

  const board = await readGoalBoard(search, limit)
  const rows = board.value.rows
  const behind = behindCount(rows)
  const current = values.status ?? ''

  return (
    <>
      <Refresher intervalMs={config().pollIntervalMs} />

      <Panel
        label="goals"
        count={behind === 0 ? undefined : <>{count(behind)} behind</>}
        error={board.error}
        action={
          <nav aria-label="Status" className="flex items-center gap-2">
            {[''].concat(STATUSES).map((status) => (
              <Link
                key={status === '' ? 'all' : status}
                href={href('/goals', withValue(values, 'status', status))}
                aria-current={current === status ? 'true' : undefined}
                className={`text-table transition-colors ${
                  current === status
                    ? 'text-warning'
                    : 'text-faint hover:text-muted-foreground'
                }`}
              >
                {status === '' ? 'all' : status}
              </Link>
            ))}
          </nav>
        }
      >
        <PaceTable
          rows={rows}
          showStatus
          caption={
            current === ''
              ? 'Goals, furthest behind pace first'
              : `Goals with status ${current}, furthest behind pace first`
          }
          empty={
            current === ''
              ? 'No goals. Nothing is being measured on this instance.'
              : `No goals with status ${current}.`
          }
        />
        <Pager
          pathname="/goals"
          values={values}
          limit={limit}
          offset={offset}
          shown={rows.length}
          total={board.value.total}
        />
      </Panel>
    </>
  )
}
