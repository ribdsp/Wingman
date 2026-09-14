import { describe, expect, it } from 'vitest'
import { tailOffset } from './thread'

describe('tailOffset', () => {
  it('returns null when the whole conversation fits in one read', () => {
    expect(tailOffset(12, 60)).toBeNull()
  })

  it('returns null when the total is exactly the window', () => {
    // Arrange: a thread of exactly 60 messages, read with limit=60.
    // Act / Assert: offset 0 already held all of them; a second read would be waste.
    expect(tailOffset(60, 60)).toBeNull()
  })

  it('lands on the last window of a long conversation', () => {
    expect(tailOffset(200, 60)).toBe(140)
  })

  it('returns null when the total is unknown', () => {
    // A body that carried no pagination gives no total. Guessing an offset from a number
    // nobody has would silently skip messages.
    expect(tailOffset(null, 60)).toBeNull()
  })

  it('returns null for a window of zero or less', () => {
    expect(tailOffset(200, 0)).toBeNull()
    expect(tailOffset(200, -10)).toBeNull()
  })
})
