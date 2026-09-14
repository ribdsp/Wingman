// Which of three colours a word from either service gets.
//
// The palette has exactly three instrument colours and one quiet default, and the mapping
// lives here rather than in components so that a decision, a stop reason and an approval
// outcome cannot end up painted differently on two screens.
//
//   attention  warning      somebody is waiting on a human, or work is in flight
//   ok         success      on track, done, allowed
//   stop       destructive  denied, halted, behind, unreadable, failed
//   quiet      plain text   routine. Most rows in a healthy console are this
//
// The vocabularies are the services' own — docs/goal-engine.md's ten decisions, docs/core.md's
// eleven stop reasons, the three approval outcomes — and an unrecognised word falls through
// to quiet rather than being guessed at. A word this console has not been taught is not
// evidence that everything is fine, and it is not evidence that anything is wrong either.

import type { PaceState } from './format'

export type Tone = 'attention' | 'ok' | 'stop' | 'quiet'

/**
 * decisionTone colours one evaluation verdict.
 *
 * `skipped_invalid_sample` is stopped, with the rest of that set, because that is what
 * it means: the metric did not produce a finite number and the engine refused to read the
 * absence as on-track. Painting it quiet would undo the reason the decision exists.
 */
export function decisionTone(decision: string): Tone {
  switch (decision) {
    case 'trigger':
      return 'attention'
    case 'cooldown_skipped':
    case 'trigger_budget_exhausted':
      // Behind pace, and deliberately throttled. Worth a person's eye — a goal that spends
      // a whole period in this state is a goal whose cooldown or budget is wrong.
      return 'attention'
    case 'noop':
    case 'achieved':
      return 'ok'
    case 'missed':
    case 'skipped_invalid_sample':
    case 'halted':
      return 'stop'
    case 'skipped_not_started':
    case 'skipped_inactive':
      return 'quiet'
    default:
      return 'quiet'
  }
}

/** outcomeTone colours a spending gate verdict: auto_approved, pending, denied. */
export function outcomeTone(outcome: string): Tone {
  switch (outcome) {
    case 'auto_approved':
      return 'ok'
    case 'pending':
      return 'attention'
    case 'denied':
      return 'stop'
    default:
      return 'quiet'
  }
}

/**
 * resolutionTone colours how an approval ended.
 *
 * `expired` is stopped rather than quiet. Nobody answered in time, so the spend did not
 * happen — the same end state as a rejection, reached by inattention instead of a decision,
 * which is worth seeing.
 */
export function resolutionTone(resolution: string | null): Tone {
  switch (resolution) {
    case 'approved':
      return 'ok'
    case 'rejected':
    case 'expired':
      return 'stop'
    default:
      return 'quiet'
  }
}

/**
 * stopTone colours one of core's eleven stop reasons.
 *
 * One of the eleven is ok and ten are stopped, which is the shape of the ladder itself:
 * `completed` is the only reason that means the work got done. `cancelled` is stopped too — a
 * person stopping a run is a legitimate act, and the run still did not finish.
 */
export function stopTone(stop: string): Tone {
  if (stop === '') return 'attention' // still running
  return stop === 'completed' ? 'ok' : 'stop'
}

/** taskTone colours a task's lifecycle status. */
export function taskTone(status: string): Tone {
  switch (status) {
    case 'succeeded':
      return 'ok'
    case 'failed':
      return 'stop'
    case 'queued':
    case 'running':
      return 'attention'
    default:
      return 'quiet'
  }
}

/**
 * goalTone colours a goal's lifecycle status.
 *
 * Two of the five carry a colour and three do not. `missed` is stopped because the period
 * ended without the target being met, and `achieved` is ok because it was. `active`, `paused`
 * and `archived` are quiet: they say what the engine is doing about a goal, not how it went,
 * and a paused goal is not a problem — it is a decision somebody made.
 */
export function goalTone(status: string): Tone {
  switch (status) {
    case 'achieved':
      return 'ok'
    case 'missed':
      return 'stop'
    default:
      return 'quiet'
  }
}

/** paceTone colours a goal row. `unknown` is not green and it is not red. */
export function paceTone(state: PaceState): Tone {
  switch (state) {
    case 'ahead':
    case 'ontrack':
      return 'ok'
    case 'behind':
      return 'stop'
    case 'unknown':
      return 'quiet'
  }
}

/**
 * auditTone colours one audit row's outcome, and mostly does not.
 *
 * The audit log is nearly all routine: created, updated, recorded, sent. Colouring those
 * would leave a screen where everything is coloured and nothing stands out, so only the
 * outcomes worth stopping on get an instrument colour and the rest stay plain.
 */
export function auditTone(outcome: string): Tone {
  switch (outcome) {
    case 'failed':
    case 'denied':
    case 'rejected':
    case 'expired':
    case 'halted':
    case 'missed':
    case 'engaged':
      return 'stop'
    case 'pending':
    case 'trigger':
      return 'attention'
    default:
      return 'quiet'
  }
}

/** TEXT is the text colour for each tone, and the only place these classes are written. */
export const TEXT: Readonly<Record<Tone, string>> = {
  attention: 'text-warning',
  ok: 'text-success',
  stop: 'text-destructive',
  quiet: 'text-muted-foreground',
}

/** MARK is the tone's colour as a background, for the 2px rule down a row. */
export const MARK: Readonly<Record<Tone, string>> = {
  attention: 'bg-warning',
  ok: 'bg-success',
  stop: 'bg-destructive',
  quiet: 'bg-input',
}
