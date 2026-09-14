import { describe, expect, test } from 'vitest'
import {
  auditTone,
  decisionTone,
  goalTone,
  outcomeTone,
  paceTone,
  resolutionTone,
  stopTone,
  taskTone,
} from './tone'

describe('the vocabularies each service publishes', () => {
  // Every decision in docs/goal-engine.md's ladder. The list is here so that adding a
  // decision to the engine without teaching the console about it fails a test rather than
  // rendering as an uncoloured word nobody notices.
  const DECISIONS = [
    'noop',
    'trigger',
    'cooldown_skipped',
    'trigger_budget_exhausted',
    'achieved',
    'missed',
    'skipped_not_started',
    'skipped_inactive',
    'skipped_invalid_sample',
    'halted',
  ] as const

  test.each(DECISIONS)('%s is a decision the console has a tone for', (decision) => {
    // Arrange / Act
    const tone = decisionTone(decision)

    // Assert: the fallback is quiet, and only two decisions are legitimately quiet.
    if (tone === 'quiet') {
      expect(['skipped_not_started', 'skipped_inactive']).toContain(decision)
    }
  })

  // Core's eleven stop reasons, from docs/core.md.
  const STOPS = [
    'completed',
    'cancelled',
    'provider_error',
    'halted',
    'budget_unreadable',
    'run_budget_exhausted',
    'user_budget_exhausted',
    'iteration_cap',
    'tool_call_cap',
    'tool_denied',
    'abandoned',
  ] as const

  test('completed is the only stop reason that reads as done', () => {
    const done = STOPS.filter((stop) => stopTone(stop) === 'ok')

    expect(done).toEqual(['completed'])
  })

  test('halted and completed are never the same colour', () => {
    // "I could not tell" is not "it is fine" — the same rule the ladder itself enforces.
    expect(stopTone('halted')).not.toBe(stopTone('completed'))
  })

  test('a run still in flight reads as attention, not as done', () => {
    expect(stopTone('')).toBe('attention')
  })
})

describe('money and pace', () => {
  test.each([
    ['auto_approved', 'ok'],
    ['pending', 'attention'],
    ['denied', 'stop'],
  ] as const)('%s is %s', (outcome, tone) => {
    expect(outcomeTone(outcome)).toBe(tone)
  })

  test('an approval nobody answered reads the same as a refusal', () => {
    expect(resolutionTone('expired')).toBe(resolutionTone('rejected'))
  })

  test('an unresolved approval has no resolution colour yet', () => {
    expect(resolutionTone(null)).toBe('quiet')
  })

  test('a goal with no evaluation is neither green nor red', () => {
    expect(paceTone('unknown')).toBe('quiet')
    expect(paceTone('behind')).toBe('stop')
    expect(paceTone('ontrack')).toBe('ok')
    expect(paceTone('ahead')).toBe('ok')
  })

  test('a broken metric feed is coloured stopped, not skipped over', () => {
    expect(decisionTone('skipped_invalid_sample')).toBe('stop')
  })
})

describe('goalTone', () => {
  test.each([
    ['achieved', 'ok'],
    ['missed', 'stop'],
  ] as const)('%s is how the period ended, and is coloured %s', (status, tone) => {
    expect(goalTone(status)).toBe(tone)
  })

  test.each(['active', 'paused', 'archived'])(
    '%s says what the engine is doing, not how it went, so it stays quiet',
    (status) => {
      // A paused goal is a decision somebody made. Colouring it would read as a fault.
      expect(goalTone(status)).toBe('quiet')
    },
  )

  test('a status the console has not been taught is quiet', () => {
    expect(goalTone('something_new')).toBe('quiet')
  })
})

describe('the audit log stays quiet', () => {
  test.each(['created', 'updated', 'recorded', 'sent', 'ok', 'released', 'approved'])(
    '%s is routine and takes no instrument colour',
    (outcome) => {
      expect(auditTone(outcome)).toBe('quiet')
    },
  )

  test.each(['failed', 'denied', 'rejected', 'expired', 'halted', 'missed', 'engaged'])(
    '%s is worth stopping on',
    (outcome) => {
      expect(auditTone(outcome)).toBe('stop')
    },
  )

  test('a word the console has not been taught is quiet, not alarming and not reassuring', () => {
    expect(auditTone('something_new')).toBe('quiet')
    expect(decisionTone('something_new')).toBe('quiet')
    expect(taskTone('something_new')).toBe('quiet')
  })
})
