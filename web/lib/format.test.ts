import { describe, expect, test } from 'vitest'
import {
  ABSENT,
  ago,
  capReading,
  clock,
  compact,
  count,
  dayStamp,
  detailLine,
  ledgerReading,
  millis,
  money,
  paceState,
  percent,
  ratio,
  remaining,
  short,
  stamp,
  words,
} from './format'

describe('absent is not zero', () => {
  test('a null count is an em dash, never 0', () => {
    expect(count(null)).toBe(ABSENT)
    expect(count(0)).toBe('0')
  })

  test('a null ratio is an em dash, never 0.00', () => {
    // 0.00 pace reads as catastrophically behind. A goal with no evaluation is not that.
    expect(ratio(null)).toBe(ABSENT)
    expect(ratio(0)).toBe('0.00')
  })

  test('a pace with no evaluation is unknown, not on track', () => {
    expect(paceState(null)).toBe('unknown')
  })

  test.each([
    [1.4, 'ahead'],
    [1.1, 'ahead'],
    [1.05, 'ontrack'],
    [1, 'ontrack'],
    [0.999, 'behind'],
    [0, 'behind'],
  ])('pace %s is %s', (value, expected) => {
    // The 1.0 boundary is the engine's, not the console's.
    expect(paceState(value)).toBe(expected)
  })
})

describe('ledgerReading', () => {
  test('an unreadable ledger says so in words rather than showing zero', () => {
    // A run whose ledger cannot be read stops. "0 today" would suggest the opposite.
    expect(ledgerReading({ isReadable: false, tokensToday: 0 })).toEqual({
      text: 'unreadable',
      state: 'unreadable',
    })
  })

  test('a readable ledger renders the number', () => {
    expect(ledgerReading({ isReadable: true, tokensToday: 42_000 })).toEqual({
      text: '42,000',
      state: 'value',
    })
  })

  test('a genuine zero spend renders as zero', () => {
    expect(ledgerReading({ isReadable: true, tokensToday: 0 })).toEqual({
      text: '0',
      state: 'value',
    })
  })

  test('a missing flag is absent rather than assumed readable', () => {
    expect(ledgerReading({ isReadable: null, tokensToday: 5 }).state).toBe('absent')
  })
})

describe('capReading', () => {
  test('a null cap is uncapped in words', () => {
    expect(capReading(null)).toEqual({ text: 'uncapped', state: 'absent' })
  })

  test('a zero cap is rendered as zero, not smoothed into uncapped', () => {
    // In core zero means unset and the floor applies, so a zero arriving here is a bug
    // worth seeing.
    expect(capReading(0)).toEqual({ text: '0', state: 'value' })
  })
})

describe('stamp', () => {
  test('a timestamp renders in the offset the service sent, not the viewer local zone', () => {
    // The console and the audit log must agree about when something happened, whatever
    // timezone the laptop reading it is in.
    expect(stamp('2026-09-13T09:05:00+07:00')).toBe('13 Sep 09:05')
    expect(stamp('2026-09-13T09:05:07+07:00', true)).toBe('13 Sep 09:05:07')
  })

  test('the same instant written in a different offset renders as written', () => {
    expect(stamp('2026-09-13T02:05:00Z')).toBe('13 Sep 02:05')
  })

  test('an unparseable timestamp is an em dash, not Invalid Date', () => {
    expect(stamp('')).toBe(ABSENT)
    expect(stamp('yesterday')).toBe(ABSENT)
  })

  test('dayStamp groups rows by the date as written', () => {
    expect(dayStamp('2026-09-13T09:05:00+07:00')).toBe('13 Sep 2026')
  })

  test('clock is the time alone, in the offset the service wrote', () => {
    // The audit log groups by day, so the row itself only carries the time. No conversion:
    // 02:05Z stays 02:05, the same rule stamp follows.
    expect(clock('2026-09-13T09:05:07+07:00')).toBe('09:05:07')
    expect(clock('2026-09-13T02:05:00Z')).toBe('02:05:00')
    expect(clock('yesterday')).toBe(ABSENT)
  })
})

describe('ago', () => {
  const now = Date.parse('2026-09-13T09:00:00+07:00')

  test.each([
    ['2026-09-13T08:59:40+07:00', 'just now'],
    ['2026-09-13T08:58:45+07:00', '1m ago'],
    ['2026-09-13T08:52:00+07:00', '8m ago'],
    ['2026-09-13T07:00:00+07:00', '2h ago'],
    ['2026-09-11T09:00:00+07:00', '2d ago'],
  ])('%s reads as %s', (iso, expected) => {
    expect(ago(iso, now)).toBe(expected)
  })

  test('a timestamp in the future is not a negative age', () => {
    expect(ago('2026-09-13T09:01:00+07:00', now)).toBe('just now')
  })

  test('an unparseable timestamp is an em dash', () => {
    expect(ago('nonsense', now)).toBe(ABSENT)
  })
})

describe('remaining', () => {
  const now = 1_757_700_000_000

  test('lapsed authority returns null so the caller renders the lapsed state', () => {
    expect(remaining(now, now)).toBeNull()
    expect(remaining(now - 1, now)).toBeNull()
  })

  test.each([
    [30_000, 'under a minute'],
    [27 * 60_000, '27m'],
    [60 * 60_000, '1h'],
    [(4 * 60 + 12) * 60_000, '4h 12m'],
  ])('%sms remaining reads as %s', (offset, expected) => {
    expect(remaining(now + offset, now)).toBe(expected)
  })
})

describe('the rest', () => {
  test('compact keeps a heading short', () => {
    expect(compact(42_000_000)).toBe('42M')
    expect(compact(null)).toBe(ABSENT)
  })

  test('percent matches how the engine states elapsed', () => {
    expect(percent(0.5)).toBe('50%')
    expect(percent(null)).toBe(ABSENT)
  })

  test('millis scales with the duration', () => {
    expect(millis(420)).toBe('420ms')
    expect(millis(4200)).toBe('4.2s')
    expect(millis(125_000)).toBe('2m 5s')
    expect(millis(null)).toBe(ABSENT)
  })

  test('words keeps the services vocabulary rather than inventing labels', () => {
    // budget_unreadable is a documented stop reason. Renaming it in the UI would make the
    // console and docs/core.md disagree.
    expect(words('budget_unreadable')).toBe('budget unreadable')
    expect(words('skipped_invalid_sample')).toBe('skipped invalid sample')
  })

  test('short keeps enough of an id to recognise it', () => {
    expect(short('9c1e4f2a-1111-2222')).toBe('9c1e4f2a…')
    expect(short('abc')).toBe('abc')
  })

  test('short marks the cut, so a truncation cannot read as the whole id', () => {
    // The audit log's subject ids are not all uuids. `kill-switch` cut to `kill-swi` reads
    // as a flag genuinely called that.
    expect(short('kill-switch')).toBe('kill-switch')
    expect(short('bot:orbit-growth', 12)).toBe('bot:orbit-growth')
    expect(short('9c1e4f2a-1111-2222-3333-444455556666', 12)).toBe('9c1e4f2a-111…')
  })
})

describe('money', () => {
  test('an amount with no reading is an em dash, never 0.00', () => {
    // A request whose amount could not be read is not a request for nothing.
    expect(money(null, 'USD')).toBe(ABSENT)
  })

  test('two decimals always, so a column of amounts lines up', () => {
    expect(money(9, 'USD')).toBe('9.00 USD')
    expect(money(1234.5, 'USD')).toBe('1,234.50 USD')
    expect(money(0, 'USD')).toBe('0.00 USD')
  })

  test('the currency the engine sent is shown as sent, not converted or symbolised', () => {
    // The gate decides in whatever currency the policy names. Rendering IDR as `$` would be
    // a lie about the amount somebody is being asked to approve.
    expect(money(50_000, 'idr')).toBe('50,000.00 IDR')
    expect(money(50_000, 'ZZZ')).toBe('50,000.00 ZZZ')
  })

  test('a missing currency leaves the figure alone rather than guessing one', () => {
    expect(money(12.5, '')).toBe('12.50')
  })
})

describe('detailLine', () => {
  test('an audit detail becomes one line of key=value pairs', () => {
    expect(detailLine({ policy: 'ads.spend', amount: 25, currency: 'USD' })).toBe(
      'policy=ads.spend  amount=25  currency=USD',
    )
  })

  test('an empty value is an em dash, so a blank cell is not mistaken for a missing key', () => {
    expect(detailLine({ reason: '', engaged: false })).toBe(`reason=${ABSENT}  engaged=false`)
  })

  test('no detail is empty, not the word null', () => {
    expect(detailLine(null)).toBe('')
    expect(detailLine(undefined)).toBe('')
  })

  test('a nested object stays on the line as JSON rather than expanding the row', () => {
    expect(detailLine({ limits: { maxIterations: 8 } })).toBe('limits={"maxIterations":8}')
  })

  test('a long detail is truncated with an ellipsis rather than pushing the next event away', () => {
    const long = detailLine({ note: 'x'.repeat(400) })

    // 180 characters plus the ellipsis: the row stays one line, and the … says there is more.
    expect(long).toHaveLength(181)
    expect(long.endsWith('…')).toBe(true)
  })
})
