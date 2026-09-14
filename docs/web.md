# The console

The screen an operator watches. It is a Next.js app that renders what the
[goal engine](goal-engine.md) and [core](core.md) already know, and forwards the few
things an operator decides back to them.

It holds no data of its own. There is no third database, no cache and no local state
that outlives a request — every number on every screen was read from one of the two
services during that request. Which is the property that makes the console safe to
restart, safe to run two of, and safe to read as the truth: if it is on the screen, a
service said it.

What it adds instead is credential handling. Both services are API-key or
session-authenticated and neither is meant to be reachable from a browser; the console
is the thing that holds a credential on the server, decides which one a given action
needs, and never lets one reach the page.

- [Screens](#screens)
- [Three credentials](#three-credentials)
- [Operator authority](#operator-authority)
- [Sealed cookies](#sealed-cookies)
- [Reads happen on the server](#reads-happen-on-the-server)
- [The proxy](#the-proxy)
- [What the console cannot do](#what-the-console-cannot-do)
- [When a read fails](#when-a-read-fails)
- [The Content-Security-Policy](#the-content-security-policy)
- [Settings](#settings)
- [Exposure](#exposure)
- [What it deliberately does not have](#what-it-deliberately-does-not-have)
- [Working on it](#working-on-it)

## Screens

Five places in the rail, in the order they are needed during an incident rather than
alphabetically, plus three detail pages reached from them.

| | Answers | What changes the world here |
|---|---|---|
| `/` — the deck | Who is waiting on a person, what is behind pace, what the monitor just decided, what has happened, and a way to say something | Resolve an approval |
| `/goals` | Why nothing is happening on that goal — usually paused, or a period that ended quietly | — |
| `/goals/:id` | What the bar is, and every verdict the engine has recorded against it | Move a target (operator) |
| `/audit` | What that actor did, between these two times | — |
| `/chat`, `/chat/:id` | Every conversation, and one at length | Send a message |
| `/runs/:id` | Why a run stopped, step by step, and what it cost | Cancel the run |
| `/authority` | Who can currently act as me: the account, held operator authority, sessions, linked chats | Hold or drop operator authority; revoke a session or a linked chat |
| `/signin` | — | Sign in |

The deck's contents and their order are fixed by [roadmap.md](roadmap.md) item 3 and
are not a layout preference: open approval queue, then pace as a single number per
goal, then the last tick's decisions, then the audit log, then a chat. Two things on
that screen change anything — resolving an approval, and the kill switch in the strip
above it.

The strip is in the layout rather than on each page, so no screen can exist that an
operator could act from while everything was halted. It carries four readings: the
kill switch, the number of open approvals, today's token spend from core's ledger, and
the countdown on any held operator authority.

The rail is permanently open from 768px up — an ops console is a room you leave a
window open on, and a rail that hides itself on a desktop is one you have to remember
the shape of. Below 768px it becomes a drawer: it leaves the flow, translates off the
left edge, and opens over the content from a toggle in the strip, with a backdrop to
dismiss it and the panels behind it made `inert` so a keyboard cannot walk into them.
That is not a preference either — 240px of a 390px screen is 62% of the window spent
on navigation, and it left the deck's tables 150px to render eight columns of figures
into. Following a row closes it, so a phone never lands on the next screen behind the
rail.

## Three credentials

Privilege here works the way it works in both services: it comes from where the
credential lives, never from anything in a request.

| | Lives in | Reaches | Ceiling |
|---|---|---|---|
| A person's session | `wgm_session`, sealed, httpOnly | core | Their own chats, runs, ledger, sessions and linked channels — core scopes every query by `user_id` |
| The console's own key | `GOAL_ENGINE_BOT_KEY` in the server's environment | the goal engine | Reads, and **engaging** the kill switch |
| Operator authority | `wgm_operator`, sealed, httpOnly, minutes long | the goal engine | Releasing the kill switch, resolving an approval, moving a target |

`GOAL_ENGINE_BOT_KEY` is a key listed in the engine's `GOAL_ENGINE_BOT_KEYS`, not in
`GOAL_ENGINE_API_KEYS`, and the difference is the whole reason it is safe to keep in a
web server's environment. A bot key can read everything the console renders and can
engage the kill switch — the engine deliberately does not route-gate engaging, because
whoever notices the damage should be able to stop it. It cannot release the switch,
resolve an approval or lower a goal's target. The engine refuses those to a bot key in
its service, and that refusal is what this key is chosen for.

There is deliberately **no core machine key** in this app's environment at all. Every
call to core is made with the signed-in person's own session token, which means the
console cannot dispatch a task, cannot administer accounts, and cannot read one
person's chats while another is signed in. It has no credential with which to try.

## Operator authority

An operator key is never in the environment. It is pasted into `/authority` when it is
needed, verified, sealed into that browser's own cookie for `WEB_OPERATOR_TTL_MINUTES`,
and then it lapses. It does not renew on activity, the TTL is capped at eight hours,
and the countdown is on screen at all times: authority you have forgotten you are
holding is authority somebody else can use on your laptop.

Verification happens at paste time, because the alternative is discovering the key was
wrong at the moment somebody is trying to answer a spend request. The probe is
`POST /v1/approvals/<a fresh uuid>/resolve`, and it is chosen because it is the only
side-effect-free way to ask the engine "is this an operator's key?". In the engine's
service, resolving runs its operator check and validates the resolution before it ever
looks the approval up, and the audit row is written only after a successful resolve.
So a random id gives a clean answer:

| Answer | Reading |
|---|---|
| `404` | An operator's key, and there was no such approval |
| `403` | A valid key, but a bot key |
| `401` | The engine does not know this key |

Nothing is created, resolved or audited by the check. A `2xx` would mean the generated
uuid named a real open approval and has just resolved it, which cannot happen; it is
treated as a failed verification rather than a success.

Which credential a call is made with is decided in `lib/upstream.ts`, from the call's
declared authority, and is never passed in by a caller. There are three authorities:

- **`bot`** — the environment key. Every read.
- **`operator`** — the pasted key. Refused locally when none is held, so the message
  names the missing authority instead of reading as an outage.
- **`preferOperator`** — the operator key when one is held, otherwise the bot key.
  This exists for exactly one route, `PUT /v1/flags/kill-switch`, where the engine
  accepts an engage from either and refuses a release to a bot key. Sending the bot key
  when nobody holds authority is the correct behaviour there. It is not to be widened.

`verifyOperatorKey` is the one function in the console that takes a credential as an
argument. It exists so the generic call path never does — no caller can ask it to send
a key of the caller's choosing.

## Sealed cookies

Two cookies, both sealed, both `httpOnly`, `SameSite=Strict`, `path=/`, and `secure`
whenever `NODE_ENV` is `production`. There is no third one — in particular no `role` or
`isOperator` cookie, because authority here is only ever the presence of an unexpired,
openable operator seal, and a boolean a client could flip would be a boolean worth
flipping.

The seal is AES-256-GCM under `WEB_COOKIE_SECRET` (32 bytes, hex or base64), laid out
as `[version][iv][tag][ciphertext]` and base64url encoded. Two details are load-bearing:

- **The purpose is bound as additional authenticated data.** A session seal cannot be
  replayed into the operator cookie, or the reverse, because opening it under the other
  purpose fails authentication.
- **The expiry is inside the sealed bytes**, not only in the cookie attributes, and it
  is checked on open. A cookie whose `Max-Age` was stripped by a proxy is still expired.

A cookie read off a disk or out of a proxy log is therefore not a usable credential.
Rotating `WEB_COOKIE_SECRET` signs everybody out, which is the correct behaviour for a
secret whose only job is to make old cookies unreadable.

The session cookie's own lifetime comes from core's sign-in response rather than a local
constant: core owns how long its sessions live, and a cookie that outlived the token
would produce a console that looks signed in and 401s on every read.

## Reads happen on the server

Every screen is a server component. The reads are made in the Node process that holds
the credentials, and what reaches the browser is rendered output — never a key, never a
session token, never a base URL for either service.

Which is also why the console polls rather than streams. Core has no inbound route at
all: every channel connection it makes is outbound, so there is nothing to subscribe
to and no SSE endpoint to hold open. One `<Refresher>` per page calls
`router.refresh()` on `WEB_POLL_INTERVAL_MS`, the server re-runs its reads, and React
reconciles. It skips the tick while `document.hidden` and re-reads on
`visibilitychange`, so a tab left open overnight is not a load-generator.

One refresher for the page rather than a poll per panel, for the same reason the reads
are on the server: a panel that polled for itself would need a route the browser can
reach, which would mean another row in the proxy table for something the server was
already allowed to read.

## The proxy

Two route handlers, `/api/engine/[...path]` and `/api/core/[...path]`, exist for the
client components that write: the approval buttons, the kill switch, the composer, the
target form, the cancel button. They share one implementation, because the rules that
matter must not be able to differ between them.

A proxy that forwards whatever it is given is not a proxy but a credential loan. So the
table is explicit — method plus path pattern, and for the engine the authority the call
is made with. Adding a row is a decision, which is the point.

The order in `lib/proxy.ts` is the safety model:

1. A state-changing method on a cross-site call → `403`, nothing read.
2. Method-and-path not in the table → `404`, **before any credential is chosen**.
3. Body read under a 64 KiB cap.
4. Hand to `lib/upstream.ts`, which picks the credential from the rule's authority.

Step 2 before step 4 is why an unlisted path never reaches the code that holds a key.

The same-site guard reads `Sec-Fetch-Site`, which browsers send on every request. Both
cookies are already `SameSite=Strict`, so a cross-site request arrives without them and
could not spend authority anyway; this is the second lock. An absent header is allowed,
because that is `curl` against a local instance — a documented way to drive both
services, and one that has to send its own credential regardless.

A path segment standing in for `:id` must match `[A-Za-z0-9._-]{1,128}` and must not be
`.` or `..`. Segments are checked as Next hands them over, already split, so there is no
place for an encoded separator to be decoded into one after the check.

Query parameters are an allowlist too, one pattern per name, taken from the two
services' handlers rather than invented: `page`, `limit`, `offset`, `openOnly`,
`includeArchived`, `actionType`, `outcome`, `actorType`, `action`, `subjectType`,
`subjectId`, `product`, `status`, `metricKey`, `search`, `since`, `until`. An
unrecognised name is dropped rather than forwarded, and a recognised name with a value
that does not match its pattern is dropped as well. The audit filter checks values
against the same table locally, so a value the log would not accept is refused where it
was typed instead of silently having no effect.

### What is listed

Twelve goal-engine reads, all at `bot`:

```
GET  /v1/goals                        GET  /v1/flags
GET  /v1/goals/:id                    GET  /v1/flags/kill-switch
GET  /v1/goals/:id/evaluations        GET  /v1/audit
GET  /v1/evaluations/latest           GET  /v1/metrics
GET  /v1/approvals                    GET  /v1/metrics/:key
GET  /v1/approvals/:id                GET  /v1/metrics/:key/samples/latest
```

Three goal-engine writes:

```
PUT   /v1/flags/kill-switch     preferOperator
POST  /v1/approvals/:id/resolve operator
PATCH /v1/goals/:id             operator
```

And twenty-one core routes, every one of them as the signed-in person: `/v1/me` and
its ledger and sessions, the chat and message routes, the task and run reads plus
`POST /v1/runs/:id/cancel`, and the channel list with its link codes.

### What is absent, and why

| Route | Why not |
|---|---|
| `POST /v1/goals`, `POST /v1/approvals` | The console is a place to watch and decide, not a place to author goals or file spend requests |
| `POST /v1/metrics/:key/samples` | A writable metric reachable from a page is a falsifiable target |
| `POST /v1/monitor/tick` | A tick is the worker's job. A button that ran one would let a refresh loop trigger agent work |
| `POST /v1/approvals/expire` | A maintenance sweep, not an interaction |
| `POST /api/v1/tasks` | Dispatching unattended work is the goal engine's job. Core refuses it to a person anyway; not having the row means the console never asks |
| Account administration | Operator-only in core, and this console holds no core machine key at all |
| `POST /v1/auth/register` | Registration is closed by default and the first account is made by `core createuser` |
| `PUT /v1/me/password`, `DELETE /v1/me/sessions`, `POST /v1/auth/signout` | All three end every session the caller has, including the one the console is holding. Proxied, they would leave a live cookie behind and 401 every subsequent read. They belong to a handler that also clears the cookie — `/api/session`, which is not the proxy |
| `POST /v1/notifications` | The goal engine's outbound edge, not a button. Core refuses it to a person, the recipient is fixed to the unattended owner rather than named by a caller, and a page that could send one could announce a decision that never happened |

## What the console cannot do

Deliberately, and by the same rule as everywhere else in Wingman: the things that would
be dangerous from a browser have no path from one.

- **Create a metric definition.** They exist only in `config/metrics.yaml`, carry SQL
  and the names of env vars holding database passwords, and there is no API that writes
  one — in either service.
- **Grant a tool, add an MCP server, or declare an HTTP tool.** Same rule, in
  `config/tools.yaml`, `config/mcp.yaml` and `config/http-tools.yaml`.
- **Change a spending policy.** `config/policies.yaml` is read once at startup. A limit
  that can change without a deploy is a limit somebody can change quietly.
- **Dispatch a task.** No route, and no credential that would be allowed to.
- **Create an account.** `core createuser`, reading the password from stdin.

For all of those, see [deployment.md](deployment.md). They are operator work on the box,
not screens.

## When a read fails

Every panel is read independently and every read is wrapped, so one failure does not
fail the page: the panel shows what it has, with a line under its own heading saying
why. A deck that 500s because the audit query was slow tells an operator nothing during
the exact minute they opened it.

**Except the kill switch, which fails closed.** An unreadable flag reads as engaged, the
same rule the engine and core both apply. The panel error is still rendered, so the strip
can say "engaged" and "the engine did not answer" at the same time — which is the honest
reading, and the one that stops somebody acting on a screen whose brakes are unknown.

Refusals are not failures. A denied spend, a halted run, a `403` on a call that needed
operator authority: all of those are answers, and they are rendered as answers, with the
message the service wrote. Both services take care that a 500 body carries no driver
text, DSN, query or host name, so an `ApiError` message is passed through to the screen
as it arrived; anything that is not one gets a generic line rather than a stack.

## The Content-Security-Policy

The policy is built per request around a fresh nonce, in `proxy.ts`, with the directives
themselves in `lib/csp.ts` so they can be tested. This is not a preference. A static
`script-src 'self'` **breaks the console silently**, and it is worth stating exactly how:

A React tree that streams sends its payload to the browser in inline `<script>` tags —
`self.__next_f.push([1, "…"])`, one per flush. Under a policy with no nonce a browser
refuses to execute them, the client never receives the payload, and React throws
hydration error #412. The page renders perfectly and then nothing on it works: the
sign-in button, the kill switch and every resolve button are dead, while the screen looks
entirely normal. That is a worse failure than no CSP at all, because it is silent.

So a nonce is generated per request and the header is set on **both** the inbound request
— which is how Next learns it and stamps it onto every script tag it emits — and the
response. `'strict-dynamic'` then covers the chunks those scripts load in turn.

```
default-src 'self'
script-src  'self' 'nonce-<per request>' 'strict-dynamic'
style-src   'self' 'unsafe-inline'
img-src     'self' data:
font-src    'self'
connect-src 'self'
frame-ancestors 'none'
base-uri 'none'
form-action 'self'
object-src 'none'
```

`'unsafe-inline'` on `script-src` was the alternative and it is not a small concession
here: this origin holds a sealed operator credential and has buttons that resolve spending
decisions. Anything that can execute in this page can press them. `style-src` keeps
`'unsafe-inline'` as a considered exception, because React inserts a route's critical CSS
as an inline `<style>` and there is no nonce path for it — a stylesheet cannot read a
cookie or call an endpoint.

`connect-src 'self'` and `font-src 'self'` are both real constraints rather than
defaults: the two services are read on the server and never from the page, and the two
faces are downloaded at build time by `next/font` and served from this origin, so a
console rendered on a box with no outbound internet looks the same as one that has it.

The prefetch case is excluded from the matcher. A prefetched payload is cached by the
router and replayed later, so a nonce baked into one would be stale by the time it was
used; the document that renders it carries its own. Static assets and images are excluded
too — they execute nothing, and `X-Content-Type-Options` and `Referrer-Policy` still reach
them from `next.config.ts`.

## Settings

`web/.env.example` is the full surface. Copy it to `.env.local` for a local run, or to
`deploy/.env.web` for the compose stack.

| | Default | |
|---|---|---|
| `GOAL_ENGINE_BASE_URL` | — | Required. Reached from the server, so a private address is correct |
| `CORE_BASE_URL` | — | Required |
| `GOAL_ENGINE_BOT_KEY` | — | Required. A key from the engine's `GOAL_ENGINE_BOT_KEYS` |
| `WEB_COOKIE_SECRET` | — | Required. 32 bytes, hex or base64: `openssl rand -base64 32` |
| `WEB_OPERATOR_TTL_MINUTES` | `30` | Floor 1, ceiling 480 |
| `WEB_POLL_INTERVAL_MS` | `4000` | Floor 1000 |
| `WEB_UPSTREAM_TIMEOUT_MS` | `15000` | Floor 1000 |
| `PORT` | `3000` | |
| `HOSTNAME` | `127.0.0.1` | Loopback. Put your own TLS in front |

Configuration is read once and validated all at once, the way
`goal-engine/internal/config` does it: every problem is collected and reported together,
rather than failing on the first one and making an operator fix five things in five
restarts. The check happens the first time a request needs the configuration — Next owns
the boot, so there is no startup hook to fail in — which means a misconfigured console
starts, then answers 500 on every screen until it is fixed rather than quietly talking to
nothing. The message that names the variables is
`wingman web is not configured: …`, and it goes to **the console's log**, not to the
browser: Next replaces a server-side error with an opaque page on purpose, and this one
would otherwise tell an anonymous visitor which credentials the box is missing. A
`CHANGE_ME` placeholder counts as absent, and the message names the variables and never
their values, because it ends up in a log.

`publicConfig()` exists so that no component is ever tempted to reach for `config()` —
which holds two credentials — from code that might end up in a bundle. It returns the
poll interval and nothing else.

## Exposure

Bind it to loopback and put it behind whatever TLS and access control you already run.
Nothing in the console expects to face the internet directly.

One consequence worth being explicit about: **rate limiting is core's, and core sees the
console's address for every attempt.** Its per-IP bucket for sign-in is therefore shared
by everyone using the console. That still bounds the total attempt rate, which is what
stops password guessing — alongside argon2id — but a flood through the console consumes
the shared budget. A console reachable from the internet belongs behind the same thing
you use to keep other traffic off the box.

The console does not forward `X-Forwarded-For`. Whether an address may be claimed by a
proxy is core's own trust setting (`TRUSTED_PROXIES`), not something this app gets to
assert. It does forward the browser's `User-Agent`, on sign-in only, so that core's
"your devices" list shows a browser rather than this console's HTTP client for
everybody.

## What it deliberately does not have

- **No schema library.** `lib/envelope.ts` parses the shared envelope by hand, in one
  file of narrowing. The house rule about a dependency needing a reason that survives
  "could this be 20 lines?" applies to the console as much as to the Go services, and the
  console owns no schema — it renders two APIs that validate their own input and answer
  with field-level errors it passes through.
- **No second validation of a write.** A body is forwarded to a service that checks it
  properly. A weaker copy here would be a second rule to keep in step with two Go
  services.
- **No browser tests.** Every test under `web/lib` is a pure function: the proxy
  allowlist, the envelope parser, the sealing, the pace and thread maths, the formatters,
  the tone table, the CSP. A component that only arranges what a server component already
  fetched has nothing to assert that the type checker does not.
- **No client-side data fetching of reads.** See [above](#reads-happen-on-the-server).
- **No `NEXT_PUBLIC_*` anything.** There is no setting a browser needs that is not
  already in the rendered output.

## Working on it

```bash
cd web
npm ci
npm run typecheck     # tsc --noEmit, over the pages too
npm run test          # vitest, lib/**/*.test.ts
npm run build         # also a second type check, and proves no route went static
npm run dev
```

A page that cached its render would show an operator an approval queue from some earlier
minute, so every route is dynamic and the build output is the check: everything should be
`ƒ` except `/_not-found` and `/icon.svg`.

The console can be built and type-checked with both services down. It cannot be
*usefully* run that way — every screen would render its panels with an unreachable note
and the strip would read the kill switch as engaged, which is the correct behaviour and
not a state to develop against.

Three rules for changes here. The suite enforces the first two; the third is a review
matter, because no test can tell a second visual vocabulary from a first one:

1. **A new upstream call needs a row in `lib/proxy-routes.ts` only if a client component
   makes it.** A server component reads through `lib/upstream.ts` directly. If you find
   yourself adding a row so a page can read something, the read is in the wrong place.
2. **The CSP is load-bearing.** `lib/csp.test.ts` asserts that the nonce reaches
   `script-src` and that `script-src` never acquires `'unsafe-inline'`. If a script is
   being blocked, the answer is a nonce, not a wider policy.
3. **A new panel does not get a new look.** The console's whole visual vocabulary is
   `components/chrome.tsx` — a panel, a reading, a chip, a coloured word, a table, an
   empty state, a failure note — and a screen is assembled from those. Fixing spacing
   there fixes every screen at once; fixing it on one page starts a second vocabulary.
   The palette, the radius scale, the bot's face and the chat treatment are adapted from
   Rakazo under Apache-2.0; [NOTICE](../NOTICE) says what was taken and what changed.
