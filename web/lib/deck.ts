// The reads this console does on the server, and what it does when one fails.
//
// Two things run through every function here:
//
//  1. A failed panel does not fail the page. Each read is wrapped, and what comes back is
//     the value plus a line explaining why it is stale. A deck that 500s because the audit
//     log is slow is a deck that tells an operator nothing during the exact incident they
//     opened it for.
//  2. Except the kill switch, which fails closed. An unreadable flag reads as engaged,
//     the same rule the engine and core both apply. The panel error is still shown, so the
//     screen can say "engaged" and "the engine did not answer" at the same time.
//
// Credentials are not chosen here. callEngine takes the authority a call needs and picks
// the key itself; callCore is always the signed-in person. See lib/upstream.ts.

import { ApiError, arr } from './envelope'
import { config } from './env'
import { joinPace, lastTick, type PaceRow } from './pace'
import { upstreamPath } from './proxy-routes'
import { operatorExpiresAt, readSession } from './session'
import { tailOffset } from './thread'
import {
  type Approval,
  type AuditEvent,
  type Chat,
  type EvaluationReading,
  type Goal,
  type LedgerReadingRaw,
  type Message,
  readEvaluation,
  readLedger,
  readSwitch,
  SWITCH_UNREADABLE,
  type SwitchReading,
  type User,
} from './types'
import { callCore, callEngine } from './upstream'

/** Panel is a read that may have failed, with the last thing worth rendering. */
export type Panel<T> = {
  value: T
  /** Empty when the read succeeded. */
  error: string
}

/**
 * note turns a thrown thing into one line for a panel heading.
 *
 * An ApiError's message is already written for a person — both services take care that a
 * 500 body carries no driver text, DSN or host name — so it is passed through. Anything
 * else is a bug in this console and says so without leaking a stack.
 */
export function note(error: unknown): string {
  if (error instanceof ApiError) return error.message
  return 'The console could not complete that read.'
}

/** attempt runs a read and degrades to a fallback rather than throwing. */
export async function attempt<T>(fallback: T, load: () => Promise<T>): Promise<Panel<T>> {
  try {
    return { value: await load(), error: '' }
  } catch (error) {
    return { value: fallback, error: note(error) }
  }
}

/**
 * readViewer returns the signed-in person, or null.
 *
 * Null covers both "no cookie" and "core rejected the token" — a session revoked from
 * another device, or one that expired between the cookie's own lifetime and core's. The
 * cookie is deliberately not cleared here: a server component may not write cookies, and
 * a stale seal is harmless because every read it authorises answers 401 and lands back on
 * the sign-in screen, which overwrites it.
 */
export async function readViewer(): Promise<User | null> {
  try {
    const parsed = await callCore<User>('/v1/me')
    return parsed.data
  } catch (error) {
    if (error instanceof ApiError && (error.status === 401 || error.status === 403)) return null
    throw error
  }
}

/** Identity is the name the rail shows. Display only — it authorises nothing. */
export type Identity = { displayName: string; email: string }

/**
 * readIdentity resolves who is signed in, and keeps the console usable when core is not.
 *
 * Three outcomes, and the middle one is the interesting case:
 *
 *   no cookie, or core rejects the token   null, and the layout sends them to sign in
 *   core does not answer at all            the claims sealed in this browser's own cookie
 *   core answers                           core's account, which is the truth
 *
 * The fallback is display only and authorises nothing: every read this console makes is
 * still authorised by core, and while core is down they all fail — visibly, in the strip.
 * The reason for it is that half this console is the goal engine, and an operator whose
 * chat service has fallen over still needs the kill switch, the approval queue and the pace
 * board. Refusing to render any of it because the name in the corner cannot be confirmed
 * would be the wrong trade.
 */
export async function readIdentity(): Promise<Identity | null> {
  const session = await readSession()
  if (!session) return null
  try {
    const parsed = await callCore<User>('/v1/me')
    return { displayName: parsed.data.displayName, email: parsed.data.email }
  } catch (error) {
    if (error instanceof ApiError && (error.status === 401 || error.status === 403)) return null
    return { displayName: session.displayName, email: session.email }
  }
}

/**
 * Shell is what the status strip shows on every page: is anything moving, is anyone
 * waiting, what has been spent, and what authority is being held.
 */
export type Shell = {
  switch: SwitchReading
  switchError: string
  openApprovals: number | null
  approvalsError: string
  ledger: LedgerReadingRaw
  ledgerError: string
  /** Epoch millis when held operator authority lapses, or null when none is held. */
  operatorExpiresAt: number | null
  /** Passed to the client so no component has to reach for config(), which holds keys. */
  pollIntervalMs: number
}

/**
 * readShell reads the four things the strip needs, in parallel.
 *
 * Parallel because they are independent and one of them is on the far side of a second
 * service: serially, a slow ledger would delay the kill switch, which is the one reading
 * nobody should wait for.
 */
export async function readShell(): Promise<Shell> {
  const [flag, open, ledger, expiresAt] = await Promise.all([
    attempt<SwitchReading>(SWITCH_UNREADABLE, async () => {
      const parsed = await callEngine<unknown>('/v1/flags/kill-switch', 'bot')
      return readSwitch(parsed.data)
    }),
    attempt<number | null>(null, async () => {
      // limit=1 because only the count is wanted; the items are the queue panel's read.
      const parsed = await callEngine<unknown>('/v1/approvals?openOnly=true&limit=1', 'bot')
      return parsed.pagination?.totalItems ?? null
    }),
    attempt<LedgerReadingRaw>({ isReadable: null, tokensToday: null }, async () => {
      const parsed = await callCore<unknown>('/v1/me/ledger')
      return readLedger(parsed.data)
    }),
    operatorExpiresAt(),
  ])

  return {
    switch: flag.value,
    switchError: flag.error,
    openApprovals: open.value,
    approvalsError: open.error,
    ledger: ledger.value,
    ledgerError: ledger.error,
    operatorExpiresAt: expiresAt,
    pollIntervalMs: config().pollIntervalMs,
  }
}

/** Queue is the open approval queue: the rows, and how many there are in total. */
export type Queue = {
  items: Approval[]
  total: number | null
}

/**
 * readApprovals reads the open queue, newest first as the engine orders it.
 *
 * Only open ones. A resolved approval is history and belongs in the audit log; a queue
 * that mixed the two would make "how many are waiting" a thing you count by eye.
 */
export async function readApprovals(limit = 25): Promise<Panel<Queue>> {
  return attempt<Queue>({ items: [], total: null }, async () => {
    const parsed = await callEngine<Approval[]>(
      `/v1/approvals?openOnly=true&limit=${limit}`,
      'bot',
    )
    return {
      items: Array.isArray(parsed.data) ? parsed.data : [],
      total: parsed.pagination?.totalItems ?? null,
    }
  })
}

/** Board is every goal's pace, plus the decisions from the most recent round. */
export type Board = {
  rows: PaceRow[]
  tick: EvaluationReading[]
}

/**
 * readBoard reads active goals and their latest verdicts.
 *
 * Active only. A paused, achieved, missed or archived goal has no pace anybody can act on,
 * and the engine's evaluator skips it — /goals lists all five statuses for the times the
 * question is "why is nothing happening on that one".
 *
 * The two reads are paired deliberately: /v1/evaluations/latest returns one row per goal
 * and omits a goal it has never assessed, so the join in lib/pace.ts keeps every goal and
 * marks the unevaluated ones unknown instead of dropping them.
 */
export async function readBoard(limit = 50): Promise<Panel<Board>> {
  return attempt<Board>({ rows: [], tick: [] }, async () => {
    const [goals, evaluations] = await Promise.all([
      callEngine<Goal[]>(`/v1/goals?status=active&limit=${limit}`, 'bot'),
      callEngine<unknown>(`/v1/evaluations/latest?limit=${limit}`, 'bot'),
    ])
    const readings = arr(evaluations.data).map(readEvaluation)
    return {
      rows: joinPace(Array.isArray(goals.data) ? goals.data : [], readings),
      tick: lastTick(readings),
    }
  })
}

/** Log is a page of the audit log. */
export type Log = {
  items: AuditEvent[]
  total: number | null
}

/**
 * readAudit reads a page of the audit log, filtered.
 *
 * `search` is a raw query string — Next hands one over on every navigation — and it goes
 * through upstreamPath, the same allowlist the browser-facing proxy uses. One table of
 * recognised parameters, so a filter that works on /audit works through /api/engine and a
 * name neither service reads is dropped in both places.
 */
export async function readAudit(search = '', limit = 50): Promise<Panel<Log>> {
  return attempt<Log>({ items: [], total: null }, async () => {
    const params = new URLSearchParams(search)
    if (!params.has('limit')) params.set('limit', String(limit))
    const path = upstreamPath(['v1', 'audit'], params.toString())
    const parsed = await callEngine<AuditEvent[]>(path, 'bot')
    return {
      items: Array.isArray(parsed.data) ? parsed.data : [],
      total: parsed.pagination?.totalItems ?? null,
    }
  })
}

/** readChats reads the signed-in person's live conversations, most recent first. */
export async function readChats(limit = 30): Promise<Panel<Chat[]>> {
  return attempt<Chat[]>([], async () => {
    const parsed = await callCore<Chat[]>(`/v1/chats?limit=${limit}`)
    return Array.isArray(parsed.data) ? parsed.data : []
  })
}

/** Thread is one conversation and the tail of its messages. */
export type Thread = {
  chat: Chat | null
  messages: Message[]
  /** True when older messages exist above what is rendered. */
  truncated: boolean
}

/**
 * readThread reads a conversation's most recent messages.
 *
 * Core returns messages oldest first, which is the right order to read but the wrong end
 * to page from: offset 0 is the beginning of the conversation. So the first read learns the
 * total from the envelope's pagination, and a long thread costs one more read to land on
 * the last window. Short threads — nearly all of them — cost one.
 */
export async function readThread(chatId: string, window = 60): Promise<Panel<Thread>> {
  return attempt<Thread>({ chat: null, messages: [], truncated: false }, async () => {
    const [chat, first] = await Promise.all([
      callCore<Chat>(`/v1/chats/${chatId}`),
      callCore<Message[]>(`/v1/chats/${chatId}/messages?limit=${window}`),
    ])

    const offset = tailOffset(first.pagination?.totalItems ?? null, window)
    let messages = Array.isArray(first.data) ? first.data : []

    if (offset !== null) {
      const tail = await callCore<Message[]>(
        `/v1/chats/${chatId}/messages?limit=${window}&offset=${offset}`,
      )
      messages = Array.isArray(tail.data) ? tail.data : messages
    }

    return { chat: chat.data, messages, truncated: offset !== null }
  })
}

/**
 * readDeckChat reads the conversation the deck's chat panel shows.
 *
 * The most recent one, not a list: the panel is one of five and the question it answers is
 * "say one thing", not "which conversation was that in" — that is what /chat is for.
 *
 * No conversation at all is not an error. The composer still works and the first send creates
 * one, so this returns an empty thread carrying whatever the chat-list read had to say, and
 * the panel renders a working composer rather than a complaint.
 */
export async function readDeckChat(window = 20): Promise<Panel<Thread>> {
  const chats = await readChats(1)
  const latest = chats.value[0]
  if (latest === undefined) {
    return { value: { chat: null, messages: [], truncated: false }, error: chats.error }
  }
  return readThread(latest.id, window)
}
