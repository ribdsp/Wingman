// Moving through a long list.
//
// Links, not buttons: the position belongs in the URL with the filter, so a page of the audit
// log can be sent to someone else and be the same page when they open it. That also keeps the
// reading on the server, where the credentials are.
//
// The range is spelled out — "51–100 of 342" — because offset paging over a log that is still
// being written is approximate by nature. An operator who sees the count move between pages
// should be able to tell that is what happened.
//
// Presentational: a server component renders this.

import Link from 'next/link'
import { buttonClass } from './chrome'
import { count } from '@/lib/format'
import { href, queryString, type Values } from '@/lib/search'

type Props = {
  pathname: string
  /** The filter this position belongs to. Its own offset is replaced, not appended to. */
  values: Values
  limit: number
  offset: number
  /** How many rows came back. Fewer than the limit means this is the last page. */
  shown: number
  /** The envelope's total, when the service sent one. */
  total: number | null
}

export function Pager({ pathname, values, limit, offset, shown, total }: Props) {
  const at = (position: number): string => {
    const next: Record<string, string> = { ...values }
    if (position <= 0) delete next.offset
    else next.offset = String(position)
    return href(pathname, queryString(next))
  }

  const hasPrevious = offset > 0
  // Two ways to know there is no next page, and either is enough: the service told us the
  // total, or it sent back a short page. Without both, a list whose service omits the total
  // would offer a page that renders empty.
  const hasNext = shown >= limit && (total === null || offset + shown < total)

  const first = shown === 0 ? 0 : offset + 1
  const last = offset + shown

  return (
    <nav aria-label="Pages" className="flex items-center gap-3 pt-2">
      <span className="num text-table text-faint">
        {count(first)}–{count(last)}
        {total !== null && <> of {count(total)}</>}
      </span>
      <span className="ml-auto flex items-center gap-2">
        {hasPrevious ? (
          <Link href={at(offset - limit)} className={buttonClass()} rel="prev">
            Previous
          </Link>
        ) : (
          <span className="text-table text-faint">Previous</span>
        )}
        {hasNext ? (
          <Link href={at(offset + limit)} className={buttonClass()} rel="next">
            Next
          </Link>
        ) : (
          <span className="text-table text-faint">Next</span>
        )}
      </span>
    </nav>
  )
}
