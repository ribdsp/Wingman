// The core proxy. Same shape as the engine one, and the same table decides what may pass;
// the difference is that every call here is made as the signed-in person's own session, so
// there is no authority for a rule to declare and none for this file to choose.

import { proxy } from '@/lib/proxy'

type Context = { params: Promise<{ path: string[] }> }

async function handle(request: Request, context: Context): Promise<Response> {
  const { path } = await context.params
  return proxy('core', request, path)
}

export const GET = handle
export const POST = handle
export const PUT = handle
export const PATCH = handle
export const DELETE = handle
