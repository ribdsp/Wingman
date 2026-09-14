import { describe, expect, test } from 'vitest'
import { contentSecurityPolicy } from './csp'

/** Directives, split back out so a test can assert one without matching the whole string. */
function directives(nonce: string): Map<string, string> {
  const map = new Map<string, string>()
  for (const part of contentSecurityPolicy(nonce).split('; ')) {
    const space = part.indexOf(' ')
    if (space === -1) continue
    map.set(part.slice(0, space), part.slice(space + 1))
  }
  return map
}

describe('contentSecurityPolicy', () => {
  test('the nonce reaches script-src, because without it the console silently stops working', () => {
    // Next streams its payload in inline <script> tags. A policy with no nonce blocks
    // them, React fails to hydrate, and every button on a page that still renders
    // perfectly is dead. Verified against a running server; this is the regression guard.
    expect(directives('abc123').get('script-src')).toBe(
      "'self' 'nonce-abc123' 'strict-dynamic'",
    )
  })

  test('script-src never allows inline', () => {
    // The one directive worth a test of its own. This origin holds a sealed operator
    // credential and has buttons that resolve spend requests; a script that can be
    // injected into the page can press them.
    expect(directives('abc123').get('script-src')).not.toContain('unsafe-inline')
    expect(contentSecurityPolicy('abc123')).not.toContain('unsafe-eval')
  })

  test('nothing off-origin can be loaded or connected to', () => {
    const d = directives('abc123')
    expect(d.get('default-src')).toBe("'self'")
    // The two services are read on the server. A page that could reach them directly
    // would be a page that could be made to reach somewhere else.
    expect(d.get('connect-src')).toBe("'self'")
    // Fonts are downloaded at build time and served from here, so no CDN is listed.
    expect(d.get('font-src')).toBe("'self'")
    expect(d.get('img-src')).toBe("'self' data:")
  })

  test('the page cannot be framed, rebased, or used to post elsewhere', () => {
    const d = directives('abc123')
    // Clickjacking a kill switch is a real attack on this particular screen.
    expect(d.get('frame-ancestors')).toBe("'none'")
    expect(d.get('base-uri')).toBe("'none'")
    expect(d.get('form-action')).toBe("'self'")
    expect(d.get('object-src')).toBe("'none'")
  })

  test('a missing nonce throws rather than producing a policy that blocks everything', () => {
    // `'nonce-'` would be a syntactically valid directive that matches no script, so the
    // failure would look exactly like the bug this function exists to prevent.
    expect(() => contentSecurityPolicy('')).toThrow(/nonce/)
  })
})
