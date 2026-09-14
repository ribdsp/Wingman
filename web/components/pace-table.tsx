// Every goal's pace as a single number, and the decisions from the last tick.
//
// Nothing here computes anything. Pace, expected value, elapsed fraction and the verdict all
// come from the evaluation the engine recorded — see docs/goal-engine.md, "Do not recompute
// pace". If this table did its own arithmetic it would eventually disagree with the audit log
// about why an agent was woken, and the audit log is the one that is right.
//
// A goal with no evaluation is rendered unknown rather than at zero pace. Zero would read as
// catastrophically behind, and "nobody has assessed this yet" is a different thing that wants
// a different colour.
//
// Presentational: server components render these directly, and the numbers are as they came.

import Link from 'next/link'
import { Empty, Id, Mark, TABLE, TD, TD_WIDE, TH, Word } from './chrome'
import { count, percent, ratio, short, stamp } from '@/lib/format'
import type { PaceRow } from '@/lib/pace'
import { decisionTone, goalTone, paceTone } from '@/lib/tone'
import type { EvaluationReading } from '@/lib/types'

type PaceProps = {
  rows: readonly PaceRow[]
  /**
   * The table's own description, for a screen reader.
   *
   * A prop because the deck shows active goals and /goals shows whatever the filter asked
   * for. One hardcoded caption would be a sentence that is true on one screen and wrong on
   * the other, and wrong is worse than absent when it is the only description there is.
   */
  caption?: string
  /** What to say when the filter matched nothing. */
  empty?: string
  /**
   * Show each goal's lifecycle status.
   *
   * Off on the deck, where every row is active by construction and the column would be one
   * word repeated down the screen. On for /goals, whose reason to exist — see readGoalBoard —
   * is usually a goal that is *not* active, and where a mixed list without it would leave
   * "why is nothing happening on that one" unanswerable.
   */
  showStatus?: boolean
}

export function PaceTable({
  rows,
  caption = 'Active goals, furthest behind pace first',
  empty = 'No active goals. Nothing is being measured.',
  showStatus = false,
}: PaceProps) {
  if (rows.length === 0) {
    return <Empty>{empty}</Empty>
  }

  return (
    <table className={TABLE}>
      <caption className="sr-only">{caption}</caption>
      <thead>
        <tr>
          <th className={TH} scope="col">
            goal
          </th>
          <th className={TH} scope="col">
            metric
          </th>
          {showStatus && (
            <th className={TH} scope="col">
              status
            </th>
          )}
          <th className={`${TH} text-right`} scope="col">
            pace
          </th>
          <th className={`${TH} text-right`} scope="col">
            observed
          </th>
          <th className={`${TH} text-right`} scope="col">
            expected
          </th>
          <th className={`${TH} text-right`} scope="col">
            target
          </th>
          <th className={`${TH} text-right`} scope="col">
            elapsed
          </th>
          <th className={TH} scope="col">
            last verdict
          </th>
          <th className={TH} scope="col">
            assessed
          </th>
        </tr>
      </thead>
      <tbody>
        {rows.map(({ goal, evaluation, state }) => (
          <tr key={goal.id}>
            <td className={TD_WIDE}>
              <span className="flex items-center gap-1.5">
                <Mark tone={paceTone(state)} />
                <Link href={`/goals/${goal.id}`} className="text-foreground hover:underline">
                  {goal.title}
                </Link>
                <span className="text-faint">{goal.product}</span>
              </span>
            </td>
            <td className={`${TD} num text-muted-foreground`}>{goal.metricKey}</td>
            {showStatus && (
              <td className={TD}>
                <Word value={goal.status} tone={goalTone(goal.status)} />
              </td>
            )}
            {/* The one number the row is read for, so it is the one that carries the colour. */}
            <td className={`${TD} num text-right ${state === 'behind' ? 'text-destructive' : 'text-foreground'}`}>
              {ratio(evaluation ? evaluation.paceRatio : null)}
            </td>
            <td className={`${TD} num text-right text-muted-foreground`}>
              {count(evaluation ? evaluation.observedValue : null)}
            </td>
            <td className={`${TD} num text-right text-muted-foreground`}>
              {count(evaluation ? evaluation.expectedValue : null)}
            </td>
            <td className={`${TD} num text-right text-muted-foreground`}>
              {count(goal.targetValue)} <span className="text-faint">{goal.comparator}</span>
            </td>
            <td className={`${TD} num text-right text-faint`}>
              {percent(evaluation ? evaluation.elapsedRatio : null)}
            </td>
            <td className={TD}>
              {evaluation === null ? (
                <span className="text-faint">never evaluated</span>
              ) : (
                <Word value={evaluation.decision} tone={decisionTone(evaluation.decision)} />
              )}
            </td>
            <td className={`${TD} text-faint`}>
              {evaluation === null ? '—' : stamp(evaluation.evaluatedAt)}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}

/**
 * TickTable is what the monitor decided on its most recent pass.
 *
 * Reconstructed from evaluation timestamps rather than read from a round record, because
 * neither service keeps one — see lastTick in lib/pace.ts. It answers the question a deck is
 * opened for after something happened: what did it decide, just now, and why.
 */
export function TickTable({ tick }: { tick: readonly EvaluationReading[] }) {
  if (tick.length === 0) {
    return <Empty>The monitor has not evaluated anything yet.</Empty>
  }

  return (
    <table className={TABLE}>
      <caption className="sr-only">Decisions from the most recent monitor pass</caption>
      <thead>
        <tr>
          <th className={TH} scope="col">
            decision
          </th>
          <th className={`${TH} text-right`} scope="col">
            pace
          </th>
          <th className={TH} scope="col">
            reason
          </th>
          <th className={TH} scope="col">
            goal
          </th>
          <th className={TH} scope="col">
            at
          </th>
        </tr>
      </thead>
      <tbody>
        {tick.map((evaluation) => (
          <tr key={`${evaluation.goalId}-${evaluation.evaluatedAt}`}>
            <td className={TD}>
              <span className="flex items-center gap-1.5">
                <Mark tone={decisionTone(evaluation.decision)} />
                <Word value={evaluation.decision} tone={decisionTone(evaluation.decision)} />
              </span>
            </td>
            <td className={`${TD} num text-right text-muted-foreground`}>{ratio(evaluation.paceRatio)}</td>
            {/* The engine writes this sentence when it decides. Shown verbatim: it is the
                same string the audit log holds, and paraphrasing it here would make two
                accounts of one decision. */}
            <td className={TD_WIDE}>
              <span className="text-muted-foreground">{evaluation.reason}</span>
            </td>
            <td className={TD}>
              <Link href={`/goals/${evaluation.goalId}`} className="hover:text-foreground">
                <Id>{short(evaluation.goalId)}</Id>
              </Link>
            </td>
            <td className={`${TD} text-faint`}>{stamp(evaluation.evaluatedAt, true)}</td>
          </tr>
        ))}
      </tbody>
    </table>
  )
}
