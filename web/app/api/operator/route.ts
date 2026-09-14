// Holding, checking and dropping goal-engine operator authority.
//
// The operator key is not in this server's environment, on purpose. An environment variable
// is authority the console holds permanently, and permanent authority to move a goal's
// target or resolve a spend request is exactly what the engine's two route-gated operations
// exist to prevent. So the operator pastes it, it is verified, and it is sealed into their
// own short-lived httpOnly cookie.
//
// The key is never returned. GET answers only whether authority is held and when it lapses.

import { ApiError, isRecord, str } from '@/lib/envelope'
import { inboundRequestId, isSameSite, readBody } from '@/lib/proxy'
import { fail, ok } from '@/lib/respond'
import { clearOperatorKey, operatorExpiresAt, writeOperatorKey } from '@/lib/session'
import { verifyOperatorKey } from '@/lib/upstream'

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

/** GET reports whether authority is held, and until when. Never what it is. */
export async function GET(request: Request): Promise<Response> {
  const requestId = inboundRequestId(request.headers.get('x-request-id'))
  const expiresAt = await operatorExpiresAt()
  return ok({ held: expiresAt !== null, expiresAt }, requestId)
}

/**
 * POST takes a pasted key and holds it.
 *
 * Verified before it is sealed, with the side-effect-free probe in lib/upstream.ts. A key
 * that turns out to be a bot key is refused with a message saying so, rather than held and
 * discovered to be useless at the moment somebody is trying to answer a spend request.
 */
export async function POST(request: Request): Promise<Response> {
  const requestId = inboundRequestId(request.headers.get('x-request-id'))
  const refused = guard(request, requestId)
  if (refused) return refused

  try {
    const body = await readBody(request)
    const key = isRecord(body) ? str(body.key).trim() : ''
    if (key === '') {
      throw new ApiError({
        status: 400,
        code: 'VALIDATION_ERROR',
        message: 'Paste an operator key.',
      })
    }

    const kind = await verifyOperatorKey(key)
    if (kind === 'insufficient') {
      throw new ApiError({
        status: 403,
        code: 'FORBIDDEN',
        message: 'That key works, but the engine holds it as a bot key. Operator operations need a key listed in GOAL_ENGINE_API_KEYS.',
        requestId,
      })
    }
    if (kind === 'unknown') {
      throw new ApiError({
        status: 401,
        code: 'UNAUTHORIZED',
        message: 'The engine does not recognise that key.',
        requestId,
      })
    }

    const expiresAt = await writeOperatorKey(key)
    return ok({ held: true, expiresAt }, requestId)
  } catch (error) {
    return fail(error, requestId)
  }
}

/** DELETE drops authority now, ahead of its expiry. */
export async function DELETE(request: Request): Promise<Response> {
  const requestId = inboundRequestId(request.headers.get('x-request-id'))
  const refused = guard(request, requestId)
  if (refused) return refused

  await clearOperatorKey()
  return ok({ held: false, expiresAt: null }, requestId)
}
