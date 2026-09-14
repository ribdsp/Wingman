// Which window of a conversation to read.
//
// Core returns a chat's messages oldest first — the right order to read, and the wrong end
// to page from, because offset 0 is the beginning of the conversation rather than the part
// somebody is looking at. So a long thread costs one extra read to land on the tail.
//
// This is here, pure and tested, because both sides need the same arithmetic: the server
// renders the first paint of a thread and the browser polls it afterwards. Two copies of an
// off-by-one would show a different last message depending on which one ran.

/**
 * tailOffset is the offset of the last `window` messages, or null when the first read
 * already returned all of them.
 *
 * Null for an unknown total as well: pagination is absent from a body that failed to carry
 * it, and guessing an offset from a total nobody has would skip messages.
 */
export function tailOffset(total: number | null, window: number): number | null {
  if (total === null || window <= 0) return null
  if (total <= window) return null
  return total - window
}
