'use client'

// The audit log's filter.
//
// The filter lives in the URL, not in this component's head. That is the point: an operator
// who finds the row that explains an incident sends the address to someone else and they see
// the same rows. It also means the reading stays on the server, where the credentials are —
// this component navigates, it does not fetch.
//
// Values are checked here against the same table the proxy uses (paramAccepted), so a value
// the engine's audit filter would not accept is refused with its name said out loud rather
// than pushed into the URL and silently dropped one layer down.
//
// A new filter drops the offset. Applying a narrower filter while still on page 4 of the old
// one is the fastest way to conclude there are no matching events.

import { useState } from 'react'
import { usePathname, useRouter } from 'next/navigation'
import { buttonClass, INPUT } from './chrome'
import { paramAccepted } from '@/lib/proxy-routes'

/** The engine's audit filter, field by field. Its handler reads exactly these. */
const FIELDS = [
  { name: 'actorType', label: 'actor', hint: 'operator' },
  { name: 'action', label: 'action', hint: 'goal.updated' },
  { name: 'subjectType', label: 'subject', hint: 'approval' },
  { name: 'subjectId', label: 'subject id', hint: '' },
  { name: 'outcome', label: 'outcome', hint: 'denied' },
  { name: 'since', label: 'since', hint: '2026-09-01T00:00:00Z' },
  { name: 'until', label: 'until', hint: '' },
] as const

type Draft = Record<string, string>

type Props = {
  /** The filter currently applied, as the page received it. */
  values: Readonly<Record<string, string>>
}

export function AuditFilter({ values }: Props) {
  const router = useRouter()
  const pathname = usePathname()
  const [draft, setDraft] = useState<Draft>(() => seed(values))
  const [refused, setRefused] = useState<readonly string[]>([])

  const apply = () => {
    const query = new URLSearchParams()
    const rejected: string[] = []

    for (const field of FIELDS) {
      const value = (draft[field.name] ?? '').trim()
      if (value === '') continue
      if (!paramAccepted(field.name, value)) {
        rejected.push(field.label)
        continue
      }
      query.set(field.name, value)
    }

    // Page size survives a filter change; the position in the old result set does not.
    const limit = values.limit ?? ''
    if (limit !== '' && paramAccepted('limit', limit)) query.set('limit', limit)

    setRefused(rejected)
    if (rejected.length > 0) return

    const search = query.toString()
    router.push(search === '' ? pathname : `${pathname}?${search}`)
  }

  const clear = () => {
    setDraft({})
    setRefused([])
    router.push(pathname)
  }

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault()
        apply()
      }}
      className="flex flex-col gap-1 pb-2"
    >
      <div className="flex flex-wrap items-end gap-2">
        {FIELDS.map((field) => (
          <span key={field.name} className="flex w-44 flex-col gap-0.5">
            <label htmlFor={`filter-${field.name}`} className="label">
              {field.label}
            </label>
            <input
              id={`filter-${field.name}`}
              name={field.name}
              value={draft[field.name] ?? ''}
              onChange={(event) =>
                setDraft((current) => ({ ...current, [field.name]: event.target.value }))
              }
              placeholder={field.hint}
              autoComplete="off"
              spellCheck={false}
              className={INPUT}
            />
          </span>
        ))}
        <button type="submit" className={buttonClass()}>
          Apply
        </button>
        <button type="button" onClick={clear} className={buttonClass()}>
          Clear
        </button>
      </div>
      {refused.length > 0 && (
        <p role="alert" className="text-destructive">
          The log does not accept that value for {refused.join(', ')}.
        </p>
      )}
    </form>
  )
}

/** seed keeps only the fields this filter owns, so an unrelated parameter is not echoed back. */
function seed(values: Readonly<Record<string, string>>): Draft {
  const draft: Draft = {}
  for (const field of FIELDS) {
    const value = values[field.name]
    if (value !== undefined) draft[field.name] = value
  }
  return draft
}
