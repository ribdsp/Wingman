import { describe, expect, test } from 'vitest'
import { ApiError, ERR_MALFORMED, arr, bool, num, parseEnvelope, str } from './envelope'

const meta = { requestId: 'req-1', timestamp: '2026-09-13T09:00:00+07:00' }

function envelope(over: Record<string, unknown> = {}) {
  return { success: true, code: 200, message: 'Listed.', data: [], meta, ...over }
}

describe('parseEnvelope', () => {
  test('a success envelope yields its data and request id', () => {
    const parsed = parseEnvelope<{ id: string }>(
      200,
      envelope({ data: { id: 'goal-1' }, message: 'Goal read.' }),
    )

    expect(parsed.data).toEqual({ id: 'goal-1' })
    expect(parsed.message).toBe('Goal read.')
    expect(parsed.requestId).toBe('req-1')
    expect(parsed.pagination).toBeNull()
  })

  test('pagination is read from meta.pagination, where the services put it', () => {
    const parsed = parseEnvelope<unknown[]>(
      200,
      envelope({ meta: { ...meta, pagination: { page: 2, limit: 50, totalItems: 87, totalPages: 2 } } }),
    )

    expect(parsed.pagination).toEqual({ page: 2, limit: 50, totalItems: 87, totalPages: 2 })
  })

  test('a partial pagination block is dropped rather than half-rendered', () => {
    const parsed = parseEnvelope<unknown[]>(
      200,
      envelope({ meta: { ...meta, pagination: { page: 1, limit: 50 } } }),
    )

    expect(parsed.pagination).toBeNull()
  })

  test('a non-2xx throws an ApiError carrying the shared error code', () => {
    expect.assertions(4)
    try {
      parseEnvelope(404, {
        success: false,
        code: 404,
        message: 'Failed',
        error: { code: 'NOT_FOUND', message: 'Goal not found.' },
        meta,
      })
    } catch (error) {
      expect(error).toBeInstanceOf(ApiError)
      const api = error as ApiError
      expect(api.status).toBe(404)
      expect(api.code).toBe('NOT_FOUND')
      expect(api.requestId).toBe('req-1')
    }
  })

  test('validation fields survive, so a form can point at the wrong input', () => {
    expect.assertions(1)
    try {
      parseEnvelope(400, {
        success: false,
        error: {
          code: 'VALIDATION_ERROR',
          message: 'Failed',
          fields: { resolution: 'must be approved or denied', note: 'too long' },
        },
        meta,
      })
    } catch (error) {
      expect((error as ApiError).fields).toEqual({
        resolution: 'must be approved or denied',
        note: 'too long',
      })
    }
  })

  test('a 200 that says success false is still a failure', () => {
    // The flag exists to be believed. Trusting the status alone would render an error
    // body as data.
    expect(() =>
      parseEnvelope(200, { success: false, error: { code: 'INTERNAL_ERROR', message: 'x' }, meta }),
    ).toThrow(ApiError)
  })

  test('a failing status with no error block is reported as malformed, not as internal', () => {
    expect.assertions(1)
    try {
      parseEnvelope(502, { success: false, meta })
    } catch (error) {
      expect((error as ApiError).code).toBe(ERR_MALFORMED)
    }
  })

  test('a non-object body is malformed rather than an empty page', () => {
    expect.assertions(2)
    try {
      parseEnvelope(200, 'gateway timeout')
    } catch (error) {
      expect((error as ApiError).code).toBe(ERR_MALFORMED)
      expect((error as ApiError).status).toBe(200)
    }
  })

  test('a missing meta block does not throw; the request id is simply empty', () => {
    const parsed = parseEnvelope<unknown>(200, { success: true, data: { id: 'x' } })

    expect(parsed.requestId).toBe('')
    expect(parsed.data).toEqual({ id: 'x' })
  })
})

describe('guards', () => {
  test('num keeps zero and rejects null, so uncapped and zero stay distinct', () => {
    // The whole reason num returns null: core sends maxTokensPerUserDay null for an
    // uncapped user, and 0 would read as a person who may spend nothing.
    expect(num(0)).toBe(0)
    expect(num(null)).toBeNull()
    expect(num(undefined)).toBeNull()
    expect(num('5')).toBeNull()
    expect(num(Number.NaN)).toBeNull()
    expect(num(Number.POSITIVE_INFINITY)).toBeNull()
  })

  test('bool distinguishes absent from false', () => {
    expect(bool(false)).toBe(false)
    expect(bool(undefined)).toBeNull()
    expect(bool('true')).toBeNull()
  })

  test('str never yields the word undefined', () => {
    expect(str(undefined)).toBe('')
    expect(str(42)).toBe('')
    expect(str('ok')).toBe('ok')
  })

  test('arr drops non-object entries instead of rendering holes', () => {
    expect(arr([{ id: 'a' }, null, 'x', { id: 'b' }])).toEqual([{ id: 'a' }, { id: 'b' }])
    expect(arr(undefined)).toEqual([])
  })
})
