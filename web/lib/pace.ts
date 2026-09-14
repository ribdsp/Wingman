// Joining goals to their latest verdict.
//
// Pure, and tested, because the join is where a goal can disappear. The engine deliberately
// omits a goal it has never evaluated from /v1/evaluations/latest rather than sending zeroed
// numbers, so a naive inner join would drop exactly the goals nobody has looked at yet —
// which are the ones worth seeing on a deck.
//
// Nothing here recomputes pace. Every number comes from the evaluation the engine recorded,
// so the console and the audit log cannot disagree about why an agent was woken.

import { type PaceState, paceState } from './format'
import type { EvaluationReading, Goal } from './types'

export type PaceRow = {
  goal: Goal
  /** null when the engine has never evaluated this goal. */
  evaluation: EvaluationReading | null
  state: PaceState
}

/**
 * joinPace pairs each goal with its latest evaluation, keeping every goal.
 *
 * Ordering is behind pace first, then unknown, then on track: a deck is read top-down, and
 * the rows that need a person are the ones that go at the top. Within a band the engine's
 * own order is kept — it lists goals newest first, and re-sorting inside a band would make
 * the console's idea of "first" differ from the API's for no gain.
 */
export function joinPace(goals: readonly Goal[], evaluations: readonly EvaluationReading[]): PaceRow[] {
  const latest = new Map<string, EvaluationReading>()
  for (const evaluation of evaluations) {
    if (evaluation.goalId !== '') latest.set(evaluation.goalId, evaluation)
  }

  const rows = goals.map((goal): PaceRow => {
    const evaluation = latest.get(goal.id) ?? null
    return { goal, evaluation, state: paceState(evaluation ? evaluation.paceRatio : null) }
  })

  const rank: Record<PaceState, number> = { behind: 0, unknown: 1, ontrack: 2, ahead: 3 }
  return rows
    .map((row, index) => ({ row, index }))
    .sort((a, b) => rank[a.row.state] - rank[b.row.state] || a.index - b.index)
    .map(({ row }) => row)
}

/** behindCount is the single number the goals heading carries. */
export function behindCount(rows: readonly PaceRow[]): number {
  return rows.filter((row) => row.state === 'behind').length
}

/**
 * lastTick is the decisions from the most recent evaluation round.
 *
 * "The last tick" is not a thing either service records — a monitor pass writes one
 * evaluation per goal and no round id — so it is reconstructed here as every evaluation
 * within a window of the newest one. A minute is wider than a pass over a handful of goals
 * takes and narrower than the shortest sane monitor interval.
 */
export function lastTick(evaluations: readonly EvaluationReading[], windowMs = 60_000): EvaluationReading[] {
  const times = evaluations
    .map((evaluation) => Date.parse(evaluation.evaluatedAt))
    .filter((time) => !Number.isNaN(time))
  if (times.length === 0) return []

  const newest = Math.max(...times)
  return evaluations
    .filter((evaluation) => {
      const at = Date.parse(evaluation.evaluatedAt)
      return !Number.isNaN(at) && newest - at <= windowMs
    })
    .sort((a, b) => Date.parse(b.evaluatedAt) - Date.parse(a.evaluatedAt))
}
