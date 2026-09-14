import { describe, expect, test } from 'vitest'
import { behindCount, joinPace, lastTick } from './pace'
import type { EvaluationReading, Goal } from './types'

function goal(id: string, title: string): Goal {
  return {
    id,
    product: 'wingman',
    title,
    metricKey: 'sales.revenue',
    comparator: 'gte',
    targetValue: 1000,
    baselineValue: 0,
    periodStart: '2026-09-01T00:00:00+07:00',
    periodEnd: '2026-09-30T23:59:59+07:00',
    status: 'active',
    toleranceRatio: 0.05,
    triggerCooldownSeconds: 3600,
    maxTriggersPerPeriod: 8,
    createdBy: 'operator:root',
    createdAt: '2026-09-01T08:00:00+07:00',
  }
}

function evaluation(goalId: string, paceRatio: number, evaluatedAt: string): EvaluationReading {
  return {
    goalId,
    paceRatio,
    progressRatio: 0.5,
    elapsedRatio: 0.5,
    observedValue: 500,
    expectedValue: 500,
    targetValue: 1000,
    onTrack: paceRatio >= 1,
    targetMet: false,
    decision: paceRatio >= 1 ? 'noop' : 'trigger',
    reason: 'pace 0.50 below tolerance',
    evaluatedAt,
  }
}

describe('joining goals to their latest verdict', () => {
  test('a goal the engine has never evaluated is still listed, as unknown', () => {
    // Arrange: two goals, one verdict. The engine omits the unevaluated goal entirely
    // rather than sending it at zero pace.
    const goals = [goal('g1', 'Revenue'), goal('g2', 'Signups')]
    const evaluations = [evaluation('g1', 1.2, '2026-09-13T09:00:00+07:00')]

    // Act
    const rows = joinPace(goals, evaluations)

    // Assert
    expect(rows).toHaveLength(2)
    const unknown = rows.find((row) => row.goal.id === 'g2')
    expect(unknown?.evaluation).toBeNull()
    expect(unknown?.state).toBe('unknown')
  })

  test('behind pace sorts above unknown, and unknown above on track', () => {
    const goals = [goal('ok', 'On track'), goal('none', 'Never assessed'), goal('late', 'Behind')]
    const evaluations = [
      evaluation('ok', 1.04, '2026-09-13T09:00:00+07:00'),
      evaluation('late', 0.42, '2026-09-13T09:00:00+07:00'),
    ]

    const rows = joinPace(goals, evaluations)

    expect(rows.map((row) => row.goal.id)).toEqual(['late', 'none', 'ok'])
  })

  test('the engine order is kept within a band', () => {
    const goals = [goal('a', 'A'), goal('b', 'B'), goal('c', 'C')]

    const rows = joinPace(goals, [])

    expect(rows.map((row) => row.goal.id)).toEqual(['a', 'b', 'c'])
  })

  test('an evaluation for a goal that is not listed does not invent a row', () => {
    const rows = joinPace([goal('g1', 'Revenue')], [evaluation('deleted', 0.1, '2026-09-13T09:00:00+07:00')])

    expect(rows).toHaveLength(1)
    expect(rows[0]?.state).toBe('unknown')
  })

  test('an evaluation with no goalId is ignored rather than matched to the empty id', () => {
    const orphan = { ...evaluation('', 0.1, '2026-09-13T09:00:00+07:00') }

    const rows = joinPace([goal('', 'Nameless')], [orphan])

    expect(rows[0]?.evaluation).toBeNull()
  })

  test('behindCount counts only the behind band', () => {
    const goals = [goal('a', 'A'), goal('b', 'B'), goal('c', 'C')]
    const evaluations = [
      evaluation('a', 0.3, '2026-09-13T09:00:00+07:00'),
      evaluation('b', 0.99, '2026-09-13T09:00:00+07:00'),
      evaluation('c', 1.5, '2026-09-13T09:00:00+07:00'),
    ]

    expect(behindCount(joinPace(goals, evaluations))).toBe(2)
  })
})

describe('reconstructing the last tick', () => {
  test('evaluations from the newest round are kept, older ones are not', () => {
    const evaluations = [
      evaluation('a', 1, '2026-09-13T09:00:00+07:00'),
      evaluation('b', 1, '2026-09-13T09:00:04+07:00'),
      evaluation('c', 1, '2026-09-13T08:00:00+07:00'),
    ]

    const tick = lastTick(evaluations)

    expect(tick.map((item) => item.goalId)).toEqual(['b', 'a'])
  })

  test('no evaluations means no tick, not a crash', () => {
    expect(lastTick([])).toEqual([])
  })

  test('an unparseable timestamp does not become the newest tick', () => {
    const evaluations = [
      evaluation('good', 1, '2026-09-13T09:00:00+07:00'),
      evaluation('bad', 1, 'not a timestamp'),
    ]

    const tick = lastTick(evaluations)

    expect(tick.map((item) => item.goalId)).toEqual(['good'])
  })
})
