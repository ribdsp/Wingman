import { describe, expect, test } from 'vitest'
import { DEFAULT_LIMIT, firstValues, href, queryString, windowOf, withValue } from './search'

describe('firstValues', () => {
  test('a repeated name keeps its first value', () => {
    // Both services read a filter with c.Query, which returns the first. Keeping the last
    // would make the console disagree with the answer it renders.
    expect(firstValues({ status: ['active', 'paused'] })).toEqual({ status: 'active' })
  })

  test('an empty value is dropped rather than forwarded', () => {
    expect(firstValues({ product: '', status: 'active' })).toEqual({ status: 'active' })
  })

  test('an absent value is dropped', () => {
    expect(firstValues({ product: undefined })).toEqual({})
  })

  test('an empty first value in a repeated name is dropped, not replaced', () => {
    expect(firstValues({ status: ['', 'active'] })).toEqual({})
  })
})

describe('queryString', () => {
  test('names are sorted, so one filter has one spelling', () => {
    expect(queryString({ status: 'active', actorType: 'operator' })).toBe(
      'actorType=operator&status=active',
    )
  })

  test('an empty filter is an empty string', () => {
    expect(queryString({})).toBe('')
  })

  test('a value needing escaping is escaped', () => {
    expect(queryString({ since: '2026-09-13T00:00:00+07:00' })).toBe(
      'since=2026-09-13T00%3A00%3A00%2B07%3A00',
    )
  })
})

describe('withValue', () => {
  test('changing a filter drops the position in the old result set', () => {
    // Applying a narrower filter while still on page 4 is the fastest way to conclude there
    // are no matching rows.
    expect(withValue({ status: 'active', offset: '150' }, 'status', 'paused')).toBe(
      'status=paused',
    )
  })

  test('an empty value removes the name', () => {
    expect(withValue({ status: 'active', product: 'ledger' }, 'status', '')).toBe(
      'product=ledger',
    )
  })

  test('the page size survives a filter change', () => {
    expect(withValue({ limit: '25' }, 'status', 'missed')).toBe('limit=25&status=missed')
  })

  test('the original filter is not mutated', () => {
    const values = { status: 'active' }
    withValue(values, 'status', 'paused')
    expect(values).toEqual({ status: 'active' })
  })
})

describe('href', () => {
  test('an empty filter yields a bare path', () => {
    expect(href('/audit', '')).toBe('/audit')
  })

  test('a filter is appended', () => {
    expect(href('/audit', 'outcome=denied')).toBe('/audit?outcome=denied')
  })
})

describe('windowOf', () => {
  test('an absent window is the default page, from the beginning', () => {
    expect(windowOf({})).toEqual({ limit: DEFAULT_LIMIT, offset: 0 })
  })

  test('a stated window is used', () => {
    expect(windowOf({ limit: '25', offset: '75' })).toEqual({ limit: 25, offset: 75 })
  })

  test.each([
    ['zero', '0'],
    ['negative', '-10'],
    ['past the ceiling', '5000'],
    ['fractional', '12.5'],
    ['not a number', 'all'],
  ])('a %s limit falls back to the default', (_name, limit) => {
    expect(windowOf({ limit }).limit).toBe(DEFAULT_LIMIT)
  })

  test('a negative offset starts at the beginning', () => {
    expect(windowOf({ offset: '-1' }).offset).toBe(0)
  })
})
