// Signing in and out.
//
// Not part of the proxy table, because both ends of this do something the proxy must not:
// POST seals a credential into a cookie, DELETE removes one. The table forwards; this
// handler is where the console's own state changes.
//
// The token core mints is never in a response body. It goes straight into the sealed
// httpOnly cookie, and what comes back is the account it belongs to.

import { ApiError, isRecord, str } from '@/lib/envelope'
import { inboundRequestId, isSameSite, readBody } from '@/lib/proxy'
import { fail, ok } from '@/lib/respond'
import { clearSession, readSession, writeSession } from '@/lib/session'
import { callCore } from '@/lib/upstream'

type SignedIn = {
  token: string
  expiresAt: string
  user: { id: string; email: string; displayName: string }
}

function guard(request: Request, requestId: string): Response | null {
  if (isSameSite(request.headers.get('sec-fetch-site'))) return null
  return fail(
    new ApiError({
      status: 403,
      code: 'FORBIDDEN',
      message: 'That request did not come from this console.',
      requestId,
    }),
    requestId,
  )
}

/**
 * POST signs in.
 *
 * Rate limiting is core's. Worth being clear about what that means here: core sees this
 * console's address for every attempt, so its per-IP bucket for sign-in is shared by
 * everyone using the console. That bounds the total attempt rate, which is what stops
 * password guessing (alongside argon2id), but it does mean a flood through the console
 * consumes the shared budget. A console exposed to the internet belongs behind whatever
 * the operator already uses to keep other things off it — documented in docs/web.md.
 */
export async function POST(request: Request): Promise<Response> {
  const requestId = inboundRequestId(request.headers.get('x-request-id'))
  const refused = guard(request, requestId)
  if (refused) return refused

  try {
    const body = await readBody(request)
    if (!isRecord(body)) {
      throw new ApiError({
        status: 400,
        code: 'VALIDATION_ERROR',
        message: 'Send an email address and a password.',
      })
    }

    const parsed = await callCore<SignedIn>('/v1/auth/signin', {
      method: 'POST',
      // Passed through unread. The console does not validate a password's shape: core owns
      // that rule, and a second copy here would be a second rule to keep in step.
      body: { email: str(body.email), password: str(body.password) },
      anonymous: true,
      requestId,
      // So "your devices" shows a browser rather than this console's HTTP client.
      userAgent: request.headers.get('user-agent') ?? '',
    })

    const expiresAt = Date.parse(parsed.data.expiresAt)
    if (!parsed.data.token || Number.isNaN(expiresAt)) {
      throw new ApiError({
        status: 502,
        code: 'INTERNAL_ERROR',
        message: 'Core signed in without returning a usable session.',
        requestId,
      })
    }

    await writeSession(
      {
        token: parsed.data.token,
        userId: parsed.data.user.id,
        email: parsed.data.user.email,
        displayName: parsed.data.user.displayName,
      },
      expiresAt,
    )

    // The account, not the token. The browser never needs it and never gets it.
    return ok({ user: parsed.data.user, expiresAt: parsed.data.expiresAt }, requestId)
  } catch (error) {
    return fail(error, requestId)
  }
}

/**
 * DELETE signs out.
 *
 * The cookie is cleared whether or not core answers. A sign-out that failed because the
 * service was unreachable and left the browser holding a live session would be the wrong
 * way round: locally forgetting the token is the part that protects the person at the
 * keyboard, and core expires the session on its own schedule regardless.
 */
export async function DELETE(request: Request): Promise<Response> {
  const requestId = inboundRequestId(request.headers.get('x-request-id'))
  const refused = guard(request, requestId)
  if (refused) return refused

  const session = await readSession()
  if (session) {
    try {
      await callCore('/v1/auth/signout', { method: 'POST', requestId })
    } catch {
      // Deliberately swallowed, and only here. Nothing the caller can do about it, and the
      // next line is what they asked for.
    }
  }
  await clearSession()
  return ok({ signedOut: true }, requestId)
}
