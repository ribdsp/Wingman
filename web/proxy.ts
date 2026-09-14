// The Content-Security-Policy, and the one reason it cannot be a static header.
//
// (Next calls this file convention "proxy" as of 16 — it was `middleware.ts`. It is not
// the console's own proxy: that is lib/proxy.ts, which forwards a listed request to one
// of the two services. This file only sets headers.)
//
// A React tree that streams sends its payload to the browser in inline <script> tags —
// `self.__next_f.push([1, "…"])`, one per flush. Under `script-src 'self'` a browser
// refuses to execute them, the client never receives the payload, and React throws
// hydration error #412: the page renders and then nothing on it works. Which on this
// console means the sign-in button, the kill switch and every resolve button are dead,
// while the screen looks entirely normal. That is a worse failure than no CSP at all,
// because it is silent.
//
// So the policy is built per request around a fresh nonce. Next reads the nonce out of
// the Content-Security-Policy header on the *inbound* request — which is why it is set
// on the request as well as the response — and stamps it onto every script tag it
// emits. `'strict-dynamic'` then covers the chunks those scripts load in turn.
//
// The alternative was `'unsafe-inline'`, and it is not a small concession here: this
// origin holds a sealed operator credential in a cookie and has buttons that resolve
// spending decisions. An injected string that can execute in this page can press them.
//
// The directives themselves are in lib/csp.ts, so that they can be tested.

import { NextResponse, type NextRequest } from 'next/server'
import { contentSecurityPolicy } from './lib/csp'

export function proxy(request: NextRequest): NextResponse {
  // A uuid per request, base64'd because that is the shape the header wants. Web Crypto
  // rather than node:crypto: this runs in the edge runtime, and it is the same generator.
  const nonce = btoa(crypto.randomUUID())
  const csp = contentSecurityPolicy(nonce)

  const headers = new Headers(request.headers)
  headers.set('content-security-policy', csp)

  const response = NextResponse.next({ request: { headers } })
  response.headers.set('content-security-policy', csp)
  return response
}

export const config = {
  matcher: [
    {
      // Static assets and images are skipped: they execute nothing, and running this on
      // every chunk request would be work for no property gained. nosniff and
      // Referrer-Policy still reach them, from next.config.ts.
      source: '/((?!_next/static|_next/image).*)',
      // Prefetched payloads are cached by the router and replayed later, so a nonce
      // baked into one would be stale by the time it is used. They are HTML fragments
      // rather than documents, and the document that renders them carries its own.
      missing: [
        { type: 'header', key: 'next-router-prefetch' },
        { type: 'header', key: 'purpose', value: 'prefetch' },
      ],
    },
  ],
}
