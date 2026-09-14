'use client'

// Signing in.
//
// The form posts to this console's own /api/session, not to core: the token core mints is
// sealed into an httpOnly cookie on the server and never reaches this component. That is why
// there is no "remember me" and nothing in localStorage — the browser holds no credential it
// could be persuaded to hand over.
//
// One error message for every failure, and it is core's. Whether the address is unknown, the
// password is wrong or the account is deactivated, core answers the same way on purpose; a
// console that distinguished them would turn its sign-in box into an account enumerator.

import { useState } from 'react'
import { useRouter } from 'next/navigation'
import { call, message } from '@/lib/client'

/**
 * The fields are bigger than the console's INPUT, deliberately.
 *
 * Everything inside the console is 28px because it is a reading in a dense table. This is the
 * one form somebody types their password into, often on a laptop in a hurry, and a 28px field
 * is a target you miss. 38px is the console's other scale — the one it already uses for rows
 * you point at rather than read.
 */
const FIELD =
  'h-nav w-full rounded-lg border border-input bg-background px-3 text-body text-foreground placeholder:text-faint focus:border-faint'

export function SigninForm() {
  const router = useRouter()
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  const submit = async () => {
    if (busy) return
    setBusy(true)
    setError('')
    try {
      await call('/api/session', 'POST', { email, password })
      // replace, not push: the sign-in screen should not be one Back away from a console
      // someone else is now looking at. refresh re-runs the layout, which reads the account.
      router.replace('/')
      router.refresh()
    } catch (thrown) {
      setError(message(thrown))
      setPassword('')
      setBusy(false)
    }
  }

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault()
        void submit()
      }}
      className="flex flex-col gap-3"
    >
      <div className="flex flex-col gap-1">
        <label htmlFor="email" className="label">
          email
        </label>
        <input
          id="email"
          type="email"
          value={email}
          onChange={(event) => setEmail(event.target.value)}
          autoComplete="username"
          autoFocus
          required
          className={FIELD}
        />
      </div>

      <div className="flex flex-col gap-1">
        <label htmlFor="password" className="label">
          password
        </label>
        <input
          id="password"
          type="password"
          value={password}
          onChange={(event) => setPassword(event.target.value)}
          autoComplete="current-password"
          required
          className={FIELD}
        />
      </div>

      {/* The console's only filled button, and this is the one place a filled button belongs:
          there is exactly one thing to do on this screen. Inside the console every button is
          quiet, because there a filled button would compete with the readings. */}
      <button
        type="submit"
        disabled={busy}
        className="mt-1 flex h-nav items-center justify-center rounded-lg bg-primary text-body font-medium text-primary-foreground transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-40"
      >
        {busy ? 'Signing in' : 'Sign in'}
      </button>

      {error !== '' && (
        <p role="alert" className="text-table text-destructive">
          {error}
        </p>
      )}

      {/* Registration is closed by default and the first account is made by `core createuser`,
          which reads the password from stdin. Said here so an operator who cannot get in knows
          where the account comes from instead of looking for a sign-up link. */}
      <p className="text-table text-faint">
        Accounts are made by the operator with <span className="num">core createuser</span>.
      </p>
    </form>
  )
}
