// The audit log, filtered and paged.
//
// The engine's log has no update and no delete method, so this page is the whole account of
// what happened on the instance — every goal edit, every gate decision, every kill-switch
// throw. Which is why the filter and the pager are here and not on the deck: the deck shows
// the last dozen events and cannot page past them, and the question an operator arrives with
// is usually "what did that actor do, between these two times".
//
// The filter is in the URL, so the row that explains an incident has an address somebody else
// can open. The reading stays on the server; the filter component only navigates.

import { Panel } from '@/components/chrome'
import { AuditFilter } from '@/components/audit-filter'
import { AuditTable } from '@/components/audit-table'
import { Pager } from '@/components/pager'
import { Refresher } from '@/components/refresher'
import { readAudit } from '@/lib/deck'
import { config } from '@/lib/env'
import { count } from '@/lib/format'
import { firstValues, queryString, windowOf } from '@/lib/search'

/** Events per page. Dense: this screen is read by scanning down it. */
const PER_PAGE = 60

type Props = {
  searchParams: Promise<Record<string, string | string[] | undefined>>
}

export default async function Audit({ searchParams }: Props) {
  const values = firstValues(await searchParams)
  const { limit, offset } = windowOf(values, PER_PAGE)

  // The bounded window goes back into the query string rather than the raw one being
  // forwarded: windowOf is what refuses a limit of 100000, and readAudit would otherwise
  // pass whatever the URL said straight through to the engine.
  const search = queryString({ ...values, limit: String(limit), offset: String(offset) })
  const log = await readAudit(search, limit)
  const items = log.value.items

  return (
    <>
      <Refresher intervalMs={config().pollIntervalMs} />

      <Panel
        label="audit"
        count={log.value.total === null ? undefined : <>{count(log.value.total)} events</>}
        error={log.error}
      >
        <AuditFilter values={values} />
        <AuditTable items={items} groupByDay />
        <Pager
          pathname="/audit"
          values={values}
          limit={limit}
          offset={offset}
          shown={items.length}
          total={log.value.total}
        />
      </Panel>
    </>
  )
}
