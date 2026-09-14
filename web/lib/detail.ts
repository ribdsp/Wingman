// The reads a detail page does.
//
// Split from lib/deck.ts to keep both files inside the size the house rules ask for, and
// along a real seam: deck.ts is what the first screen needs, and every function here belongs
// to one page that was navigated to on purpose.
//
// Same two rules as deck.ts. Reads are wrapped, so one failure degrades one panel rather
// than the page — a run whose cost cannot be read still shows its steps and its stop reason.
// And a page that reads three independent things reads them in parallel: serially, a slow
// audit or ledger query would delay the thing the page is actually about.

import { arr } from './envelope'
import { joinPace, type PaceRow } from './pace'
import { upstreamPath } from './proxy-routes'
import { attempt, type Panel } from './deck'
import {
  type ChannelIdentity,
  type Cost,
  type Evaluation,
  type Goal,
  readEvaluation,
  type Run,
  type RunStep,
  type Session,
  type Task,
  type User,
} from './types'
import { callCore, callEngine } from './upstream'

/** One goal, and the verdicts recorded against it. */
export type GoalDetail = {
  goal: Goal | null
  evaluations: Evaluation[]
}

/**
 * readGoalDetail reads a goal and its recent evaluations.
 *
 * Both in one panel deliberately: a goal without its history is a row of settings, and the
 * question this page answers is why the engine has been doing — or not doing — something
 * about it. The list is the engine's own order, newest first.
 */
export async function readGoalDetail(id: string, limit = 25): Promise<Panel<GoalDetail>> {
  return attempt<GoalDetail>({ goal: null, evaluations: [] }, async () => {
    const [goal, evaluations] = await Promise.all([
      callEngine<Goal>(`/v1/goals/${id}`, 'bot'),
      callEngine<Evaluation[]>(`/v1/goals/${id}/evaluations?limit=${limit}`, 'bot'),
    ])
    return {
      goal: goal.data,
      evaluations: Array.isArray(evaluations.data) ? evaluations.data : [],
    }
  })
}

/** Every goal matching a filter, with its pace. */
export type GoalBoard = {
  rows: PaceRow[]
  total: number | null
}

/**
 * readGoalBoard reads the goal list, filtered, with the latest verdict joined onto each row.
 *
 * Unlike the deck's board this does not force status=active. The reason to open this page
 * rather than read the deck is usually a goal that is not active — a paused one nobody
 * un-paused, a missed one whose period ended quietly.
 *
 * `search` is the page's raw query string, filtered through the same allowlist the proxy
 * uses, so a filter that works here works through /api/engine and an unrecognised name is
 * dropped in both places.
 */
export async function readGoalBoard(search = '', limit = 100): Promise<Panel<GoalBoard>> {
  return attempt<GoalBoard>({ rows: [], total: null }, async () => {
    const params = new URLSearchParams(search)
    if (!params.has('limit')) params.set('limit', String(limit))
    const path = upstreamPath(['v1', 'goals'], params.toString())

    const [goals, evaluations] = await Promise.all([
      callEngine<Goal[]>(path, 'bot'),
      callEngine<unknown>(`/v1/evaluations/latest?limit=${limit}`, 'bot'),
    ])

    return {
      rows: joinPace(Array.isArray(goals.data) ? goals.data : [], arr(evaluations.data).map(readEvaluation)),
      total: goals.pagination?.totalItems ?? null,
    }
  })
}

/**
 * RunDetail is one run, its steps, what it cost and the task it came from.
 *
 * Four panels rather than one value: the steps are the transcript of what happened and are
 * worth showing even when the cost read fails, and the cost is worth showing even when the
 * step list is long enough to time out.
 */
export type RunDetail = {
  run: Panel<Run | null>
  steps: Panel<RunStep[]>
  cost: Panel<Cost | null>
  task: Panel<Task | null>
}

/**
 * readRunDetail reads everything about one run.
 *
 * The run is read first because the task id comes from it — one dependent hop, and the other
 * three go in parallel. `run_steps` is append-only in core, so what comes back is the whole
 * story in order and nothing has been edited after the fact.
 */
export async function readRunDetail(id: string): Promise<RunDetail> {
  const run = await attempt<Run | null>(null, async () => {
    const parsed = await callCore<Run>(`/v1/runs/${id}`)
    return parsed.data
  })

  const taskId = run.value?.taskId ?? ''

  const [steps, cost, task] = await Promise.all([
    attempt<RunStep[]>([], async () => {
      const parsed = await callCore<RunStep[]>(`/v1/runs/${id}/steps?limit=200`)
      return Array.isArray(parsed.data) ? parsed.data : []
    }),
    attempt<Cost | null>(null, async () => {
      const parsed = await callCore<Cost>(`/v1/runs/${id}/cost`)
      return parsed.data
    }),
    attempt<Task | null>(null, async () => {
      if (taskId === '') return null
      const parsed = await callCore<Task>(`/v1/tasks/${taskId}`)
      return parsed.data
    }),
  ])

  return { run, steps, cost, task }
}

/** The signed-in person's own account: who they are, where they are signed in, what is linked. */
export type Account = {
  me: Panel<User | null>
  sessions: Panel<Session[]>
  channels: Panel<ChannelIdentity[]>
}

/**
 * readAccount reads the three things the authority page shows about a person.
 *
 * Three panels for three reads. A revoked-session list that failed to load must not hide the
 * linked channels: both are places somebody else could be holding access, and this page is
 * where that gets noticed.
 */
export async function readAccount(): Promise<Account> {
  const [me, sessions, channels] = await Promise.all([
    attempt<User | null>(null, async () => {
      const parsed = await callCore<User>('/v1/me')
      return parsed.data
    }),
    attempt<Session[]>([], async () => {
      const parsed = await callCore<Session[]>('/v1/me/sessions?limit=50')
      return Array.isArray(parsed.data) ? parsed.data : []
    }),
    attempt<ChannelIdentity[]>([], async () => {
      // No limit: core's channel list is unpaged on purpose — a person has a handful of
      // linked chats, not a page of them — and asking for one would be a parameter the
      // handler ignores, which reads like a bound that exists.
      const parsed = await callCore<ChannelIdentity[]>('/v1/channels')
      return Array.isArray(parsed.data) ? parsed.data : []
    }),
  ])

  return { me, sessions, channels }
}
