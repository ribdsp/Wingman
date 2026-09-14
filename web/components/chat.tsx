'use client'

// The chat: the one place on this console where a person talks to the agent directly.
//
// A message here takes exactly the path a goal-engine trigger takes — core's own rule, and
// the reason there is no channel-specific route around the run limits. So this composer is
// not a shortcut into the agent; it is the same door, and everything that bounds an
// unattended run bounds what is typed here.
//
// Why the messages are polled rather than streamed: core has no streaming endpoint, and that
// is deliberate (docs/core.md). A run's output is written to run_steps and the reply lands as
// a message when the run produces it, so "what is true now" is a question asked on an
// interval. The interval is the instance's own, passed down from the server.
//
// The window arithmetic is tailOffset, shared with the server's first paint. Two copies of
// that off-by-one would show a different last message before and after the first poll.
//
// Bubbles, a face and a round composer: Rakazo's chat treatment, adopted because this screen
// is the one part of the console that is a conversation rather than an instrument, and the
// rest of the console already says which side of it you are reading. The face is not
// decoration — it carries one bit that matters here, whether a run is in flight, and it is
// the same bit the "working" caption carries.

import { useCallback, useEffect, useRef, useState } from 'react'
import Link from 'next/link'
import { AGENT_COLOR, BotAvatar } from './bot-avatar'
import { Empty, Id } from './chrome'
import { SendIcon } from './icons'
import { call, get, message, usePoll } from '@/lib/client'
import { count, stamp } from '@/lib/format'
import { tailOffset } from '@/lib/thread'
import type { Message, Sent } from '@/lib/types'

/** How close to the bottom still counts as reading the end, in pixels. */
const PINNED_WITHIN = 48

/** The composer's ceiling: about six lines, then it scrolls instead of eating the screen. */
const COMPOSER_MAX_PX = 128

type Props = {
  /** The conversation being read, or null on the deck when there is not one yet. */
  chatId: string | null
  /** The tail the server rendered. The poll owns the transcript after mount. */
  initial: readonly Message[]
  pollIntervalMs: number
  /** True when the server's read left older messages above this window. */
  truncated?: boolean
  /** How many messages to hold on screen. */
  size?: number
  /** The transcript's height. Shorter on the deck, where it is one panel among five. */
  height?: string
}

export function Chat({
  chatId,
  initial,
  pollIntervalMs,
  truncated = false,
  size = 60,
  height = 'max-h-[46vh]',
}: Props) {
  // Held in state rather than read from the prop, so the deck's composer works with no
  // conversation yet: the first send creates one and this adopts its id. Navigating away to
  // the chat page mid-sentence would be the wrong answer to "say one thing".
  const [activeChatId, setActiveChatId] = useState(chatId)
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [pinned, setPinned] = useState(true)
  const scroller = useRef<HTMLDivElement | null>(null)
  const composer = useRef<HTMLTextAreaElement | null>(null)

  const load = useCallback(
    async (signal: AbortSignal): Promise<Message[]> => {
      if (activeChatId === null) return []
      const path = `/api/core/v1/chats/${activeChatId}/messages`
      const first = await get<Message[]>(`${path}?limit=${size}`, signal)
      const offset = tailOffset(first.pagination?.totalItems ?? null, size)
      if (offset === null) return Array.isArray(first.data) ? first.data : []
      const tail = await get<Message[]>(`${path}?limit=${size}&offset=${offset}`, signal)
      return Array.isArray(tail.data) ? tail.data : []
    },
    [activeChatId, size],
  )

  const { value: messages, error: pollError, refresh } = usePoll<Message[]>(
    load,
    pollIntervalMs,
    [...initial],
  )

  // Follow the end of the transcript, but only while the reader is already there. Scrolling
  // up to re-read something and being yanked back down by a poll is worse than not following
  // at all. Depends on the count rather than the array: a poll returns a new array every time
  // and only a new message should move the view.
  useEffect(() => {
    if (!pinned) return
    const element = scroller.current
    if (element) element.scrollTop = element.scrollHeight
  }, [messages.length, pinned])

  // The composer grows with what is typed, up to six lines. Measured rather than counted:
  // a pasted brief wraps, and rows= cannot know how wide the pill is. Sync with the DOM, so
  // an effect is the right tool — the height is not state anybody else reads.
  useEffect(() => {
    const element = composer.current
    if (element === null) return
    element.style.height = '0px'
    element.style.height = `${Math.min(element.scrollHeight, COMPOSER_MAX_PX)}px`
  }, [text])

  const send = async () => {
    const body = text.trim()
    if (body === '' || busy) return
    setBusy(true)
    setError('')
    try {
      const sent = await call<Sent>(
        '/api/core/v1/messages',
        'POST',
        activeChatId === null ? { text: body } : { chatId: activeChatId, text: body },
      )
      if (activeChatId === null) setActiveChatId(sent.data.chat.id)
      setText('')
      setPinned(true)
      // The message core just recorded is already in the thread, so re-read rather than
      // holding a local copy of it. One source for what the conversation contains.
      refresh()
    } catch (thrown) {
      setError(message(thrown))
    } finally {
      setBusy(false)
    }
  }

  const last = messages.length === 0 ? undefined : messages[messages.length - 1]
  const waiting = busy || last?.role === 'user'
  const failure = error !== '' ? error : pollError

  return (
    <div className="flex flex-col gap-3">
      <div
        ref={scroller}
        onScroll={(event) => {
          const element = event.currentTarget
          const distance = element.scrollHeight - element.scrollTop - element.clientHeight
          setPinned(distance < PINNED_WITHIN)
        }}
        className={`${height} overflow-y-auto`}
      >
        {truncated && <p className="label pb-2">older messages are not shown</p>}
        {messages.length === 0 ? (
          <Empty>
            {activeChatId === null
              ? 'Nothing said yet. What is typed here starts a conversation.'
              : 'This conversation has no messages.'}
          </Empty>
        ) : (
          <ol className="flex flex-col gap-4 py-1">
            {messages.map((entry) => {
              const mine = entry.role === 'user'
              return (
                <li key={entry.id} className="flex flex-col gap-1">
                  <div className={`flex gap-3 ${mine ? 'justify-end' : 'justify-start'}`}>
                    {!mine && <BotAvatar color={AGENT_COLOR} size={30} className="mt-0.5" />}
                    {/* pre-wrap: the agent writes lists, paths and short code. Collapsing its
                        whitespace would make a file listing unreadable.

                        The measure is why a bubble does not run the width of the panel. At
                        1440 the panel is ~1200px and an unmeasured line of agent prose runs
                        about 150 characters, which is twice what an eye tracks back from
                        comfortably — the reader loses their place returning to the left edge,
                        on the one screen here that is read as sentences rather than scanned
                        as a table.

                        68ch rather than the 70ch the console's other prose uses (/authority),
                        and deliberately: pre-wrap means this same paragraph may hold an
                        80-column listing, and the bubble's own padding plus 68ch of Geist
                        clears one without folding it. */}
                    <p
                      className={`max-w-[68ch] rounded-2xl px-4 py-2.5 whitespace-pre-wrap break-words ${
                        mine ? 'bg-chat-user text-foreground' : 'bg-muted text-foreground'
                      }`}
                    >
                      {entry.content}
                    </p>
                  </div>
                  {/* flex-wrap with each part unbreakable: at 390px this line does not fit,
                      and the choice is between wrapping between the parts or through the
                      middle of a timestamp. `23:34:46` alone on the next line under `13 Sep`
                      is not a reading. */}
                  <div
                    className={`flex flex-wrap items-baseline gap-x-2 text-table ${
                      mine ? 'justify-end' : 'ps-[42px]'
                    }`}
                  >
                    <span className="whitespace-nowrap text-faint">{stamp(entry.createdAt, true)}</span>
                    {entry.tokensOut > 0 && (
                      <Id>
                        {count(entry.tokensIn)} in · {count(entry.tokensOut)} out
                      </Id>
                    )}
                    {entry.runId !== undefined && entry.runId !== '' && (
                      <Link
                        href={`/runs/${entry.runId}`}
                        className="text-faint underline decoration-input hover:text-muted-foreground"
                      >
                        run
                      </Link>
                    )}
                  </div>
                </li>
              )
            })}
          </ol>
        )}

        {/* The face, working, where the reply will land. It says a run is in flight without
            claiming to know anything about it — this console cannot see a run's steps until
            the run writes them. */}
        {waiting && (
          <div className="flex items-center gap-3 py-3">
            <BotAvatar color={AGENT_COLOR} size={30} working />
            <span className="breathe text-table text-warning">working</span>
          </div>
        )}
      </div>

      <form
        onSubmit={(event) => {
          event.preventDefault()
          void send()
        }}
        className="flex flex-col gap-1.5"
      >
        <label htmlFor="composer" className="sr-only">
          Message the agent
        </label>
        <div className="flex items-end gap-2 rounded-3xl border border-border bg-background py-2 pe-2 ps-4 focus-within:border-faint">
          <textarea
            id="composer"
            ref={composer}
            rows={1}
            value={text}
            onChange={(event) => setText(event.target.value)}
            onKeyDown={(event) => {
              // Enter sends, shift-Enter breaks the line. The agent is given multi-line briefs
              // often enough that losing the newline would be the wrong trade.
              if (event.key === 'Enter' && !event.shiftKey) {
                event.preventDefault()
                void send()
              }
            }}
            placeholder={
              activeChatId === null ? 'Start a conversation. Enter sends.' : 'Enter sends, shift-Enter for a new line.'
            }
            spellCheck={false}
            className="min-h-6 flex-1 resize-none self-center bg-transparent py-1 text-body text-foreground outline-none placeholder:text-faint"
          />
          <button
            type="submit"
            aria-label="Send"
            disabled={busy || text.trim() === ''}
            className="grid h-9 w-9 shrink-0 place-items-center rounded-full bg-primary text-primary-foreground transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-30"
          >
            <SendIcon />
          </button>
        </div>
        {failure !== '' && (
          <span role="alert" className="text-table text-destructive">
            {failure}
          </span>
        )}
      </form>
    </div>
  )
}
