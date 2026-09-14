// One agent run, from the task that woke it to the reason it stopped.
//
// This is the page an incident ends on. `run_steps` is append-only in core — no update method,
// no delete — so the step list is the whole story in the order it happened, and nothing here
// summarises it. The step content is rendered in full for that reason: a run that stopped at
// `tool_denied` is explained by the line the loop actually wrote, not by a paraphrase of it.
//
// Four independent reads behind four panels. A cost query that fails must not hide the stop
// reason, and a 200-step transcript that is slow must not hide what the run cost.

import Link from 'next/link'
import { CancelRun } from '@/components/cancel-run'
import { Empty, Field, Id, Instrument, Num, Panel, TABLE, TD, TD_WIDE, TH, Word } from '@/components/chrome'
import { Refresher } from '@/components/refresher'
import { readRunDetail } from '@/lib/detail'
import { config } from '@/lib/env'
import { capReading, count, millis, stamp } from '@/lib/format'
import { stopTone, taskTone } from '@/lib/tone'

type Props = {
  params: Promise<{ id: string }>
}

export default async function RunPage({ params }: Props) {
  const { id } = await params
  const { run: runPanel, steps, cost, task } = await readRunDetail(id)
  const run = runPanel.value
  const { pollIntervalMs } = config()

  if (run === null) {
    return (
      <Panel label="run" error={runPanel.error}>
        <Empty>
          Nothing came back for <Id>{id}</Id>. Either it is not yours, or core did not answer.
        </Empty>
      </Panel>
    )
  }

  const duration = spanMs(run.startedAt, run.finishedAt)
  const dailyCap = capReading(run.limits.maxTokensPerUserDay)

  return (
    <>
      {/* Only while it is going. A finished run does not change, and a page that re-reads a
          finished transcript every few seconds is a page that costs core queries for nothing. */}
      {run.isInFlight && <Refresher intervalMs={pollIntervalMs} />}

      <Panel
        label="run"
        count={
          run.isInFlight ? (
            <span className="breathe text-warning">in flight</span>
          ) : (
            <Word value={run.stop ?? ''} tone={stopTone(run.stop ?? '')} />
          )
        }
        error={runPanel.error}
        action={
          <CancelRun runId={run.id} isInFlight={run.isInFlight} isCancelled={run.isCancelled} />
        }
      >
        <div className="flex flex-wrap gap-12 pb-4">
          <Instrument
            label="tokens"
            value={count(run.state.tokensUsed)}
            sub={<>of {count(run.limits.maxTokensPerRun)} this run · {dailyCap.text} a day</>}
          />
          <Instrument
            label="iterations"
            value={count(run.state.iterations)}
            sub={<>of {count(run.limits.maxIterations)}</>}
          />
          <Instrument
            label="tool calls"
            value={count(run.state.toolCalls)}
            sub={<>of {count(run.limits.maxToolCalls)}</>}
          />
        </div>

        <dl className="flex flex-col">
          {/* The reason string is core's own. It is what the audit row holds, and it is the
              difference between `halted` and `completed` being legible at all. */}
          {run.reason !== undefined && run.reason !== '' && (
            <Field label="reason">
              <span className="text-foreground">{run.reason}</span>
            </Field>
          )}
          <Field label="model">
            <Num>{run.provider === undefined || run.provider === '' ? '—' : run.provider}</Num>
            {run.model !== undefined && run.model !== '' && (
              <>
                <span className="text-faint"> · </span>
                <Num>{run.model}</Num>
              </>
            )}
          </Field>
          <Field label="started">
            <Num>{stamp(run.startedAt, true)}</Num>
          </Field>
          <Field label="finished">
            <Num>{run.finishedAt === null ? '—' : stamp(run.finishedAt, true)}</Num>
            {duration !== null && <span className="text-faint"> after {millis(duration)}</span>}
          </Field>
          <Field label="step timeout">
            <Num>{count(run.limits.stepTimeoutSeconds)}</Num>
            <span className="text-faint"> seconds · sandbox </span>
            <Num>{count(run.limits.sandboxTimeoutSeconds)}</Num>
            <span className="text-faint"> seconds</span>
          </Field>
          <Field label="id">
            <Id>{run.id}</Id>
          </Field>
        </dl>
      </Panel>

      <Panel
        label="task"
        error={task.error}
        action={
          task.value === null ? undefined : (
            <Word value={task.value.status} tone={taskTone(task.value.status)} />
          )
        }
      >
        {task.value === null ? (
          <Empty>The task this run came from could not be read.</Empty>
        ) : (
          <dl className="flex flex-col">
            <Field label="brief">
              <span className="whitespace-pre-wrap text-foreground">{task.value.brief}</span>
            </Field>
            <Field label="source">
              <Num>{task.value.source}</Num>
              {task.value.botId !== undefined && task.value.botId !== '' && (
                <>
                  <span className="text-faint"> · </span>
                  <Id>{task.value.botId}</Id>
                </>
              )}
              {task.value.channelId !== undefined && task.value.channelId !== '' && (
                <>
                  <span className="text-faint"> · </span>
                  <Id>{task.value.channelId}</Id>
                </>
              )}
            </Field>
            <Field label="queued">
              <Num>{stamp(task.value.createdAt, true)}</Num>
            </Field>
            <Field label="id">
              <Id>{task.value.id}</Id>
            </Field>
          </dl>
        )}
      </Panel>

      <Panel
        label="token spend"
        count={cost.value === null ? undefined : count(cost.value.total)}
        error={cost.error}
      >
        {cost.value === null || cost.value.charges.length === 0 ? (
          <Empty>No charges recorded against this run.</Empty>
        ) : (
          <table className={TABLE}>
            <caption className="sr-only">Token charges recorded against this run</caption>
            <thead>
              <tr>
                <th className={TH} scope="col">
                  at
                </th>
                <th className={TH} scope="col">
                  model
                </th>
                <th className={`${TH} text-right`} scope="col">
                  in
                </th>
                <th className={`${TH} text-right`} scope="col">
                  out
                </th>
                <th className={`${TH} text-right`} scope="col">
                  total
                </th>
              </tr>
            </thead>
            <tbody>
              {cost.value.charges.map((charge) => (
                <tr key={`${charge.occurredAt}-${charge.model}-${charge.total}`}>
                  <td className={`${TD} text-faint`}>{stamp(charge.occurredAt, true)}</td>
                  <td className={`${TD} num text-muted-foreground`}>
                    {charge.provider} · {charge.model}
                  </td>
                  <td className={`${TD} num text-right text-muted-foreground`}>{count(charge.tokensIn)}</td>
                  <td className={`${TD} num text-right text-muted-foreground`}>{count(charge.tokensOut)}</td>
                  <td className={`${TD} num text-right text-foreground`}>{count(charge.total)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <Panel
        label="steps"
        count={steps.value.length === 0 ? undefined : count(steps.value.length)}
        error={steps.error}
        action={
          <Link
            href="/audit"
            className="text-table text-muted-foreground underline decoration-input hover:text-foreground"
          >
            audit log
          </Link>
        }
      >
        {steps.value.length === 0 ? (
          <Empty>No steps recorded. The run stopped before its first iteration.</Empty>
        ) : (
          <table className={TABLE}>
            <caption className="sr-only">Every step of this run, in order</caption>
            <thead>
              <tr>
                <th className={`${TH} text-right`} scope="col">
                  #
                </th>
                <th className={TH} scope="col">
                  kind
                </th>
                <th className={`${TH} text-right`} scope="col">
                  tokens
                </th>
                <th className={TH} scope="col">
                  at
                </th>
                <th className={TH} scope="col">
                  what happened
                </th>
              </tr>
            </thead>
            <tbody>
              {steps.value.map((step) => (
                <tr key={step.index}>
                  <td className={`${TD} num text-right text-faint`}>{step.index}</td>
                  <td className={TD}>
                    <span className="text-muted-foreground">{step.kind}</span>
                    {step.toolName !== undefined && step.toolName !== '' && (
                      <>
                        {' '}
                        <Num>{step.toolName}</Num>
                      </>
                    )}
                  </td>
                  <td className={`${TD} num text-right text-faint`}>
                    {count(step.tokensIn)}/{count(step.tokensOut)}
                  </td>
                  <td className={`${TD} text-faint`}>{stamp(step.at, true)}</td>
                  {/* In full, wrapped. This column is the transcript; truncating it is how a
                      console ends up hiding the sentence that explains the stop reason. */}
                  <td className={TD_WIDE}>
                    <span className="whitespace-pre-wrap break-words text-muted-foreground">
                      {step.content}
                    </span>
                    {step.err !== undefined && step.err !== '' && (
                      <span className="mt-1 block whitespace-pre-wrap break-words text-destructive">
                        {step.err}
                      </span>
                    )}
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

/**
 * spanMs is how long a run took, or null while it is still going.
 *
 * Both ends parsed, both checked: core sends RFC 3339 and Date.parse returns NaN for anything
 * else, and NaN through millis() would render as a duration rather than as absent.
 */
function spanMs(startedAt: string, finishedAt: string | null): number | null {
  if (finishedAt === null) return null
  const start = Date.parse(startedAt)
  const end = Date.parse(finishedAt)
  if (Number.isNaN(start) || Number.isNaN(end)) return null
  return end - start
}
