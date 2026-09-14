// The goal-engine proxy. Everything the browser may ask the engine is in
// lib/proxy-routes.ts; the forwarding rules are in lib/proxy.ts. This file is only the
// binding between them and Next's router, deliberately with nothing in it.

import { proxy } from '@/lib/proxy'

type Context = { params: Promise<{ path: string[] }> }

async function handle(request: Request, context: Context): Promise<Response> {
  const { path } = await context.params
  return proxy('engine', request, path)
}

export const GET = handle
export const POST = handle
export const PUT = handle
export const PATCH = handle
export const DELETE = handle
