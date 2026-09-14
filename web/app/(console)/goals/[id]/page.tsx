// One goal: what it is measured against, and every verdict the engine has recorded on it.
//
// The two halves answer different questions. The settings say what the bar is; the verdicts say
// what the engine has been doing about it, and that is where an operator finds out that nothing
// has happened because the cooldown is a week, or because the metric stopped producing samples
// and the evaluator has been refusing to read the absence as on-track.
//
// Nothing here recomputes pace. Every number is the one the engine wrote when it decided.

import { notFound } from 'next/navigation'
import { Empty, Field, Id, Mark, Num, Panel, TABLE, TD, TD_WIDE, TH, Word } from '@/components/chrome'
import { Refresher } from '@/components/refresher'
import { TargetForm } from '@/components/target-form'
import { readGoalDetail } from '@/lib/detail'
import { config } from '@/lib/env'
import { count, percent, ratio, stamp } from '@/lib/format'
import { operatorExpiresAt } from '@/lib/session'
import { decisionTone, goalTone } from '@/lib/tone'

type Props = {
  params: Promise<{ id: string }>
}

export default async function GoalPage({ params }: Props) {
  const { id } = await params
  if (id === '') notFound()

  const [held, detail] = await Promise.all([operatorExpiresAt(), readGoalDetail(id)])
  const isOperator = held !== null && held > Date.now()
  const { goal, evaluations } = detail.value

  // Not notFound(): a read that failed and an id that does not exist arrive here the same way,
  // and a 404 page would tell an operator the goal is gone when the engine is merely down.
  // The panel says which, in the engine's own words.
  if (goal === null) {
    return (
      <Panel label="goal" error={detail.error}>
        <Empty>
          Nothing came back for <Id>{id}</Id>. Either no goal has that id, or the engine did not
          answer.
        </Empty>
      </Panel>
    )
  }

  return (
    <>
      <Refresher intervalMs={config().pollIntervalMs} />

      <Panel
        label={goal.title}
        error={detail.error}
        action={<Word value={goal.status} tone={goalTone(goal.status)} />}
      >
        <dl className="flex flex-col">
          <Field label="product">{goal.product}</Field>
          <Field label="metric">
            <Num>{goal.metricKey}</Num>
          </Field>
          <Field label="target">
            <span className="text-faint">{goal.comparator}</span> <Num>{count(goal.targetValue)}</Num>
          </Field>
          <Field label="baseline">
            <Num>{count(goal.baselineValue)}</Num>
          </Field>
          <Field label="period">
            <Num>{stamp(goal.periodStart)}</Num>
            <span className="text-faint"> → </span>
            <Num>{stamp(goal.periodEnd)}</Num>
          </Field>
          {/* The three below are the engine's throttles, and they are on this page because a
              goal that looks ignored is usually a goal being throttled exactly as configured. */}
          <Field label="tolerance">
            <Num>{percent(goal.toleranceRatio)}</Num>
            <span className="text-faint"> behind pace before it triggers</span>
          </Field>
          <Field label="cooldown">
            <Num>{count(goal.triggerCooldownSeconds)}</Num>
            <span className="text-faint"> seconds between triggers</span>
          </Field>
          <Field label="triggers">
            <Num>{count(goal.maxTriggersPerPeriod)}</Num>
            <span className="text-faint"> per period at most</span>
          </Field>
          {goal.botId !== undefined && goal.botId !== '' && (
            <Field label="dispatches to">
              <Id>
                {goal.botId}
                {goal.channelId !== undefined && goal.channelId !== '' && ` · ${goal.channelId}`}
              </Id>
            </Field>
          )}
          <Field label="created">
            <Num>{stamp(goal.createdAt, true)}</Num>
            <span className="text-faint"> by </span>
            <Id>{goal.createdBy}</Id>
          </Field>
          <Field label="id">
            <Id>{goal.id}</Id>
          </Field>
          {goal.sourceText !== undefined && goal.sourceText !== '' && (
            <Field label="written as">
              <span className="whitespace-pre-wrap text-muted-foreground">{goal.sourceText}</span>
            </Field>
          )}
        </dl>
      </Panel>

      <Panel label="the bar">
        <TargetForm goal={goal} isOperator={isOperator} />
      </Panel>

      <Panel
        label="verdicts"
        count={evaluations.length === 0 ? undefined : count(evaluations.length)}
      >
        {evaluations.length === 0 ? (
          <Empty>
            The engine has never evaluated this goal. Either its period has not started, or it is
            not active, or no sample has arrived for <Num>{goal.metricKey}</Num>.
          </Empty>
        ) : (
          <table className={TABLE}>
            <caption className="sr-only">Recorded verdicts on this goal, newest first</caption>
            <thead>
              <tr>
                <th className={TH} scope="col">
                  at
                </th>
                <th className={TH} scope="col">
                  decision
                </th>
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
                  progress
                </th>
                <th className={`${TH} text-right`} scope="col">
                  elapsed
                </th>
                <th className={TH} scope="col">
                  reason
                </th>
              </tr>
            </thead>
            <tbody>
              {evaluations.map((evaluation) => (
                <tr key={evaluation.id}>
                  <td className={`${TD} num text-muted-foreground`}>{stamp(evaluation.evaluatedAt, true)}</td>
                  <td className={TD}>
                    <span className="flex items-center gap-1.5">
                      <Mark tone={decisionTone(evaluation.decision)} />
                      <Word value={evaluation.decision} tone={decisionTone(evaluation.decision)} />
                    </span>
                  </td>
                  <td
                    className={`${TD} num text-right ${
                      evaluation.onTrack ? 'text-foreground' : 'text-destructive'
                    }`}
                  >
                    {ratio(evaluation.paceRatio)}
                  </td>
                  <td className={`${TD} num text-right text-muted-foreground`}>
                    {count(evaluation.observedValue)}
                  </td>
                  <td className={`${TD} num text-right text-muted-foreground`}>
                    {count(evaluation.expectedValue)}
                  </td>
                  <td className={`${TD} num text-right text-faint`}>
                    {percent(evaluation.progressRatio)}
                  </td>
                  <td className={`${TD} num text-right text-faint`}>
                    {percent(evaluation.elapsedRatio)}
                  </td>
                  {/* Verbatim, for the same reason the tick table shows it verbatim: this is the
                      string the audit log holds. */}
                  <td className={TD_WIDE}>
                    <span className="text-muted-foreground">{evaluation.reason}</span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  )
}
