// The sign-in screen, and the only page outside the console's route group.
//
// Outside because the group's layout is the gate: everything under app/(console) redirects here
// when there is no session, so this page cannot live under it without redirecting to itself.
//
// Nothing is read from either service here. A sign-in screen that called the goal engine to
// decorate itself would be a screen that fails to load when the engine is down, on the one
// occasion an operator most needs to get in.

import type { Metadata } from 'next'
import { redirect } from 'next/navigation'
import { BotAvatar } from '@/components/bot-avatar'
import { SigninForm } from '@/components/signin-form'
import { readSession } from '@/lib/session'

export const dynamic = 'force-dynamic'

export const metadata: Metadata = {
  title: 'Sign in · Wingman',
}

/** The one parameter this screen reads. See the note in Signin for why it is safe to. */
type Query = { stale?: string }

export default async function Signin({ searchParams }: { searchParams: Promise<Query> }) {
  // `stale=1` is set by the console layout when it had a cookie this app could unseal and core
  // rejected the token inside it — a revoked or expired session. The flag is what stops the two
  // gates from bouncing the browser between each other: without it this page would see a valid
  // cookie, redirect to the deck, and the deck would redirect back.
  //
  // Trusted for one thing only: whether to show this form. It grants nothing, and the worst a
  // hand-typed one does is offer a second sign-in to somebody already signed in.
  const stale = (await searchParams).stale !== undefined

  // Already signed in: go to the deck rather than offering a second sign-in, which would mint
  // a second core session and leave the first one live in the list on /authority.
  if (!stale && (await readSession()) !== null) redirect('/')

  return (
    <main className="flex min-h-dvh items-center justify-center px-6 py-16">
      {/* One card, centred. The console behind this screen is a wall of readings; the door to
          it is a single object with a single thing to do, and the shape says so. */}
      <div className="flex w-full max-w-100 flex-col gap-6">
        <div className="flex flex-col items-start gap-4">
          <BotAvatar size={44} />
          <div className="flex flex-col gap-2">
            <h1 className="text-[1.75rem] leading-tight tracking-tight text-foreground">
              Wingman acts without being asked.
            </h1>
            {/* The one sentence worth putting on this screen: what the thing behind it will do
                while nobody is looking. Not a feature list — this is an ops console, and the
                person reading it already runs the instance. */}
            <p className="text-body text-muted-foreground">
              Goals are watched, spending is gated, and every decision is written down. Signing in
              gets you the queue, the pace of each goal, and the switch that stops all of it.
            </p>
          </div>
        </div>

        <div className="flex flex-col gap-4 rounded-2xl border border-border bg-card p-5">
          {/* Said plainly, because the alternative reading is that the password was wrong.
              Nothing was: the session ended somewhere else — it was revoked, it expired, or
              core came back without it. */}
          {stale && (
            <p className="text-table text-warning">
              That session is no longer valid. Sign in again to mint a new one.
            </p>
          )}
          <SigninForm />
        </div>
      </div>
    </main>
  )
}
