// The Content-Security-Policy this console serves, built around one nonce.
//
// Here rather than in proxy.ts so it can be tested. What a test can prove about a CSP is
// narrow but exactly the thing that goes wrong silently: that the nonce is in it, and that
// script-src did not acquire 'unsafe-inline' the next time somebody hit a blocked script.
// A console whose CSP allows inline script is a console where an injected string can press
// the button that resolves a spending decision.

/**
 * contentSecurityPolicy returns the header value for one request.
 *
 * `connect-src 'self'` and nothing else: both Go services are read from the server, never
 * from the page, so a browser reaching off-origin is either a mistake or an exfiltration
 * attempt. `font-src 'self'` because next/font downloads the two faces at build time and
 * this origin serves them.
 *
 * `style-src` keeps 'unsafe-inline' and that is a considered exception: React inserts a
 * route's critical CSS as an inline <style> and there is no nonce path for it. A stylesheet
 * cannot read a cookie or call an endpoint, so it is not the same trade as script.
 *
 * `'strict-dynamic'` is what lets a nonced script load the chunks it needs; without it
 * every dynamically inserted chunk tag would have to be listed by host.
 */
export function contentSecurityPolicy(nonce: string): string {
  if (nonce === '') throw new Error('contentSecurityPolicy: nonce is required')
  return [
    "default-src 'self'",
    `script-src 'self' 'nonce-${nonce}' 'strict-dynamic'`,
    "style-src 'self' 'unsafe-inline'",
    "img-src 'self' data:",
    "font-src 'self'",
    "connect-src 'self'",
    "frame-ancestors 'none'",
    "base-uri 'none'",
    "form-action 'self'",
    "object-src 'none'",
  ].join('; ')
}
