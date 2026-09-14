# Architecture

Wingman is two Go services that share no code, only an HTTP boundary — plus a console
that is a client of both and holds nothing.

```
                      ┌──────────────────────────────────────────┐
   the console, ─────▶│  core        (this repo, Go)             │
   a chat app, curl   │  agent loop · tools · sandbox            │
   — chat and tasks   │  accounts · chats · token budgets        │
                      └──────────────────────────────────────────┘
                            │                    ▲
                            │                    │  POST /api/v1/tasks
   ask to spend, report ────┤                    │  "MRR is 12% behind pace"
   tokens, read the         │                    │
   kill switch              ▼                    │
                      ┌──────────────────────────┴───────────────┐
   the console, ─────▶│  goal-engine (this repo, Go)             │
   or curl — goals    │  goals · monitor · trigger bridge        │
   and approvals      │  approval gate · audit log               │
                      └──────────────────────────────────────────┘
                            │                    │
                    ┌───────▼──────┐     ┌───────▼─────────────┐
                    │ own Postgres │     │ your product DBs    │
                    │ (read/write) │     │ and APIs (read only)│
                    └──────────────┘     └─────────────────────┘
                            ▲
                    ┌───────┴──────┐
   core's own ──────│ own Postgres │   separate database, separate credential
   accounts, chats  │ (read/write) │
   and transcripts  └──────────────┘
```

The console (`web/`, Next.js) is drawn as a caller because that is all it is. It has no
database, no queue and no state that outlives a request; it makes the same HTTP calls
`curl` would, and every one of them happens in its server process so that no credential
and no service address reaches the browser. It is not a third Go module and nothing in
either service depends on it — delete it and the two of them are unchanged.
[docs/web.md](web.md) is its own picture; [Inside the console](#inside-the-console)
below is the short version.

## Why two services

Because they answer different questions and one of them holds the brakes.

Core decides **how far** a run may go: how many iterations, which tools, in which
sandbox, against whose token budget. The goal engine decides **whether** to act at all
and **whether** money may move. Putting both in one process means the code that runs
agent-authored output shares an address space with the code that authorises spending.

The rest follows:

- **Separate database.** The audit log and the approval queue must survive anything
  that happens to accounts, chats and transcripts, and the reverse. Neither service
  can read the other's schema; each has its own credential.
- **Separate blast radius.** Core runs agent-authored code in a sandbox and holds
  provider API keys. The service that decides whether money may move should not be in
  that process, and should not be reachable with core's credential.
- **Separate uptime.** If core is down, the goal engine keeps evaluating and keeps
  recording; the dispatch fails and is retried. If the goal engine is down, core still
  answers you — it stops being woken, and no tool call may spend, because the gate that
  would approve one is unreachable and unreachable means denied.
- **Separate deploy.** Each has its own `go.mod`, its own image, its own migrations,
  and no build-time dependency on the other. They can be a version apart.

They deliberately do **not** share a module for the ~200 lines of response envelope and
middleware they have in common. A shared module would couple two services that deploy
independently, for less code than the coupling costs — so it is duplicated, and
`CONTRIBUTING.md` says which files those are.

## What crosses the boundary

Three calls. One in each direction, plus one read.

### The goal engine tasks core

```
POST {WINGMAN_CORE_BASE_URL}{WINGMAN_CORE_TASK_PATH}      default: /api/v1/tasks
Authorization: Bearer {WINGMAN_CORE_API_KEY}
Idempotency-Key: goal:0f8c…:2026-09-11T18:00:00Z

{
  "botId": "growth",
  "channelId": "C0123",
  "brief": "MRR is 12% behind the pace needed for \"kejar MRR 50 juta sebelum akhir kuartal\" …",
  "idempotencyKey": "goal:0f8c…:2026-09-11T18:00:00Z",
  "metadata": { "goalId": "0f8c…", "metricKey": "business.mrr", "product": "acme" }
}
```

`brief` is the instruction in your own words — the goal's `sourceText`, quoted, with
the numbers attached. `metadata` is context, not authority: every policy decision is
made in the goal engine and none of it is delegated to the agent reading this.

The key is a **bot** key over in core (`CORE_BOT_KEYS`), not an operator one. Filing a
task and reading what you filed is all the dispatch needs.

`idempotencyKey` travels as a header as well as in the body, and core enforces it with a
unique index — a response lost in transit and retried produces the same task id rather
than a second run. It is a header rather than a query parameter so it stays out of
access logs.

`WINGMAN_CORE_TASK_PATH` is configurable because the two services deploy independently:
a route that moves over there should be a config edit here, not a redeploy of this one.
`TRIGGER_DRY_RUN=true` logs and records the task instead of sending it, which is how a
new deployment is meant to spend its first week.

### Core reads the kill switch

```
GET {GOAL_ENGINE_BASE_URL}/v1/flags/kill-switch
Authorization: Bearer {GOAL_ENGINE_API_KEY}
```

Before every run and between iterations. Fail-closed, the same rule the engine applies
to itself: unreadable means engaged, engaged means halt. Leave `GOAL_ENGINE_BASE_URL`
empty and core runs standalone — no kill switch, and no tool call may spend money.

### Core asks to spend, and reports what it spent

A tool call classified as spending files a request with the engine's existing ladder
(`POST /v1/approvals`) and waits for the answer. Core has no second, weaker gate: the
limits live in operator-owned YAML the engine reads once at startup, and a limit that
could change without a deploy is a limit somebody can change quietly.

Each finished run also pushes its token usage as a sample
(`POST /v1/metrics/ops.tokens_spent/samples`), so cost becomes a metric a goal can be
written against.

All three use the same **bot** key: core asks permission to spend, it does not grant
it. The credential in `GOAL_ENGINE_API_KEY` belongs in the engine's
`GOAL_ENGINE_BOT_KEYS`, and putting an operator key there would hand the service that
asks the credential that approves.

## Inside the goal engine

```
cmd/goal-engine        process: config, wiring, workers, shutdown
  │
internal/handler       HTTP: bind, render, status codes          ← gin
internal/middleware    request id, recover, access log, rate limit, auth
  │
internal/service       orchestration, authorisation, audit writes
  │
internal/repository    SQL. One type per table, parameterised queries
internal/database      pool, migrations
  │
internal/domain        the decision logic. No I/O, no clock, no database
```

Plus three that sit to the side: `internal/metrics` (the metric registry and the
samplers that read SQL and HTTP sources), `internal/policy` (the spending policy
registry), and `internal/core` (the client that talks to core).

Two rules hold this together.

**The domain is pure.** `internal/domain` decides whether a goal is behind pace,
what a spend request should get, and what a trigger brief says. It takes values and
returns values — no database handle, no `time.Now()`, no HTTP client. That is why
the evaluator is tested against two dozen hand-written scenarios instead of a live
Postgres, and why "what would this goal do tomorrow" is a function call.

**Services own the I/O, behind small ports.** Every dependency a service has is an
interface declared in `internal/service/ports.go` — `GoalStore`, `SampleReader`,
`AuditSink`, `MetricLookup`, `TaskCreator`, `Clock`, and a dozen more, most with
one or two methods. The repositories satisfy them; so do the fakes in the tests.
Nothing in `internal/service` imports `gin`, and nothing in `internal/handler`
writes SQL.

### Where authorisation lives

In two places, deliberately.

The router installs `middleware.RequireOperator` on the two routes that need a
human. The service *also* checks, through `Actor.requireOperator`. A route guard
protects a route; the service guard protects the invariant if a second route, a CLI,
or a scheduled job ever reaches the same operation.

`Actor` is an explicit parameter — `{Type, ID, RequestID}` — passed down from the
handler, never smuggled through `context.Context`. A caller's identity is an
argument to a decision, not ambient state, and every audit row is written from it.

Roles are fixed at startup by which environment variable listed the credential:
`GOAL_ENGINE_API_KEYS` is operators, `GOAL_ENGINE_BOT_KEYS` is bots. There is no
request field, header, or body that can raise a role.

### What one tick does

```
for each active goal, oldest deadline first, at most MONITOR_MAX_GOALS_PER_TICK:

  kill switch engaged? ────────────────────────── yes ─▶ decision: halted, stop
  sample the metric  (push: read the latest stored value)
                     (sql:  one read in a read-only transaction, timeout-bounded)
                     (http: one GET, jsonPath into the body)
  no sample, or older than METRIC_MAX_SAMPLE_AGE? ─────▶ skipped_invalid_sample
  domain.Evaluate(goal, sample, now)
    ├── target met, or period closed ────────────▶ achieved / missed  (goal closed)
    ├── within tolerance of the pace line ───────▶ noop
    └── behind ──▶ cooldown elapsed? budget left? ─▶ trigger  ─▶ core
                                          else ───▶ cooldown_skipped /
                                                    trigger_budget_exhausted
  write the evaluation row, and an audit row for anything that acted
```

Every branch writes an evaluation row, including the ones that did nothing. A
monitor that only records its interventions cannot answer "was it watching?".

A tick is bounded by the monitor interval, so two ticks never overlap and race each
other's cooldowns. Its first run waits one full interval after startup — a
crash-looping process would otherwise re-sample every metric on every boot. When
you want an answer now, `POST /v1/monitor/tick`.

### Data

One database, eight tables:

| Table | Holds |
|---|---|
| `goals` | targets, periods, cooldowns, trigger budgets, status |
| `metric_samples` | the time series behind every evaluation |
| `goal_evaluations` | one row per goal per tick, decision included |
| `trigger_dispatches` | what was sent to core, and whether it arrived |
| `approval_requests` | the spending queue, with idempotency keys |
| `spend_ledger` | what was actually committed, for the daily cap |
| `audit_events` | append-only, every actor and every action |
| `system_flags` | the kill switch |

Enums are real Postgres enums, so an invalid decision or outcome cannot be stored
even by a hand-written `UPDATE`.

## Inside core

The same layering, the same two rules:

```
cmd/core               process: config, wiring, run workers, sweeps, shutdown
  │                    plus two subcommands: createuser, healthcheck
internal/handler       HTTP: bind, render, status codes          ← gin
internal/middleware    request id, recover, access log, rate limit, api key, session
  │
internal/service       orchestration, authorisation, audit writes
  │
internal/repository    SQL. One type per table, parameterised queries
internal/database      pool, migrations
  │
internal/domain        the decision logic. No I/O, no clock, no database
```

Plus seven leaf packages that services reach through ports and that know nothing about
HTTP:

| | |
|---|---|
| `internal/provider` | One port, two implementations: Anthropic and OpenAI, both official SDKs. `DEFAULT_PROVIDER` picks one and only that provider's key is required. |
| `internal/tool` | The registry, and three tool sources: a shell tool that runs in the sandbox, MCP servers, and HTTP endpoints an operator declared. All three read operator-owned YAML. |
| `internal/sandbox` | One port, two backends. `docker` starts a throwaway container per command; `local` runs on this host and says in its own source that it is not isolation. |
| `internal/agent` | The loop, and the prompt it builds. |
| `internal/auth` | argon2id at OWASP parameters, and opaque session tokens stored as a hash. Also the link codes, hashed the same way. |
| `internal/channel` | One port, three adapters — Telegram, Slack, Discord — and a hub that owns their lifetimes. Each normalises its platform's events into one `Inbound`; none of them decides anything. |
| `internal/goalengine` | The client for the goal engine: kill switch, approvals, spend reporting. |

Every channel connection is **outbound**: Telegram long polling, Slack's Socket Mode
socket, Discord's gateway. That is a deployment property as much as a design one —
there is no inbound webhook route on either service, so nothing about connecting a chat
platform needs a public address, a certificate or a signature check. A missing
credential means that adapter is not built, and a platform that is unreachable at
startup does not stop core booting.

### Where authorisation lives

Three kinds of caller, and which kind you are is fixed at startup by where your
credential lives — never by anything in the request:

| | Credential | May |
|---|---|---|
| **operator** | an entry in `CORE_API_KEYS` | cancel anyone's run, read anyone's transcript, read the whole run log |
| **bot** | an entry in `CORE_BOT_KEYS` | file a task, read what it filed. In practice: the goal engine's trigger bridge |
| **person** | a row in `users` plus a live session | everything scoped to themselves, and nothing outside it |

An environment key is never a user. A user session can never dispatch a system task.
Operator-only operations are checked twice, at the route and again in the service, for
the same reason the goal engine does it.

Every query over user content carries `user_id` in the `WHERE` clause rather than
filtering after the read, and a sandbox workspace belongs to one person and is never
shared. Sessions are opaque random tokens stored as a SHA-256 hash, so a database dump
is not a set of live sessions; sign-in pays the argon2id cost even for an address with
no account, so response time does not say whether an account exists.

Unattended work — a task from the goal engine, with no person behind it — is filed
against the account named in `CORE_UNATTENDED_OWNER`, resolved to an id once at boot. A
run with no owner is a run whose tokens appear in no ledger and whose transcript nobody
can find, so the variable is required as soon as a bot key is set and has no default.

### What one run does

```
a task is claimed by a run worker
  │
  ├─ kill switch engaged, or unreadable? ──────────── yes ─▶ halted, stop
  │
  ├─ snapshot the tool grants for this run   (immutable for its lifetime: a grant
  │                                           edited mid-run does not widen it)
  │
  └─ loop, at most RUN_MAX_ITERATIONS times:
       │
       ├─ domain.Decide(state, limits, ledger, killSwitch, now)
       │     eleven reasons, in the order in docs/core.md. Anything but
       │     "continue" ends the run and is recorded as the stop reason.
       │
       ├─ ask the provider, bounded by RUN_STEP_TIMEOUT and by the tokens left
       │
       ├─ for each tool call it asked for:
       │     domain.ClassifyTool(grants, name, attended)
       │       ├─ allowed ─────────▶ execute (shell → sandbox, MCP → server,
       │       │                     http → the declared endpoint), bounded by
       │       │                     SANDBOX_TIMEOUT
       │       ├─ spend ───────────▶ file with the goal engine's gate and wait
       │       ├─ unattended write ▶ refuse this call, tell the model why
       │       └─ denied ──────────▶ stop the run: tool_denied
       │
       └─ append a run step. Append only — no update, no delete
  │
  └─ report the run's token usage to the goal engine as a metric sample
```

Every iteration and every tool call is a `run_steps` row, including the refused ones. A
run log that only records what succeeded cannot answer "what did it try?".

Two sweeps run alongside the workers, once at startup and then every five minutes: one
deletes expired sessions, the other closes work a dead worker left behind — a run still
in `running` is failed as `abandoned` (the eleventh stop reason, which exists because a
run that stops being touched must not sit in `running` forever), and its task is
requeued. How long a run may be in flight before that happens is derived from the
operator's own limits rather than configured separately, because the only correct answer
is "longer than a legitimate run":
`2 × MaxIterations × (StepTimeout + SandboxTimeout)`. An operator who allows two hundred
iterations of fifteen minutes has runs that take days, and a fixed timeout would requeue
them while they were still working.

### Core's data

Its own database, ten tables:

| Table | Holds |
|---|---|
| `users` | accounts: email, argon2id hash, name, status |
| `sessions` | opaque tokens, stored hashed, with an expiry |
| `chats` | a conversation, per user — and, for a channel thread, which platform and which conversation it is |
| `messages` | its turns |
| `tasks` | work to do, with a unique index on `idempotency_key` |
| `runs` | one attempt at a task: limits, counters, stop reason |
| `run_steps` | append-only: every iteration, every tool call, every refusal |
| `token_spend` | what a run used, and the per-person daily total behind the cap |
| `channel_identities` | a Telegram, Slack or Discord account linked to a Wingman one |
| `channel_link_codes` | short-lived single-use codes, stored hashed, that prove one |

A channel conversation is a `chats` row rather than a table mapping conversations to
chats, because that table would hold one row per chat and nothing else — the
conversation *is* the chat, and the alternative is a join on every message to answer
"which thread is this". The unique index on it is scoped by `user_id`, so a group
conversation holding two linked people gives each their own thread, their own
transcript and their own budget; one shared row would put one person's agent output in
the other's history.

Enums again real Postgres enums: `task_status`, `task_source`, `run_stop_reason`,
`step_kind`, `message_role`, `channel_kind`. A stop reason outside the eleven cannot be
stored.

There is no `tool_grants` table. Grants live in `config/tools.yaml`, read at startup,
because a list of what an autonomous agent may do is not a list an autonomous agent may
edit — and a table is reachable by anything holding a database credential.

## Inside the console

```
proxy.ts               one header: a Content-Security-Policy with a fresh nonce
  │
app/(console)/         the screens: five in the rail, three detail pages. Server
                       components, and the layout is the gate in front of all of them
app/signin/            the one page that takes a password, deliberately outside the group
app/api/               four routes: sign in/out, paste/clear an operator key,
                       and two proxies — one per service
  │
components/            client components: the buttons, the filters, the poller
  │
lib/upstream.ts        the only module that makes an outbound request
lib/proxy-routes.ts    the allowlist: which upstream paths a browser may cause
lib/session.ts         sealed cookies in, a credential out
lib/seal.ts            AES-256-GCM, purpose bound as AAD, expiry inside the seal
lib/deck.ts lib/pace.ts lib/envelope.ts …   pure: parse, derive, format
```

No database, no ORM, no cache, no state between requests. What the console adds to the
two services is **credential handling**, and that is the only reason it is a server at
all rather than a static bundle:

| Credential | Where it lives | What it can do |
|---|---|---|
| a person's core session | sealed in an httpOnly cookie | everything that person may do in core |
| the goal engine's **bot** key | the console's environment | read every engine screen; engage the kill switch |
| an **operator** key | nowhere until pasted, then a second sealed cookie for 30 minutes | resolve an approval, release the switch, move a target |

The third row is the point. `GOAL_ENGINE_OPERATOR_KEY` does not exist in this app: a
long-lived operator credential in a web server's environment is one server compromise
away from being an operator. A pasted key is verified against the engine with a
side-effect-free probe, sealed, and expires without renewal.

Two rules hold the rest together.

**Every read happens on the server.** Pages are server components, so what reaches the
browser is rendered output — never a key, never a session token, never either service's
base URL. It follows that the console polls rather than streams: core has no inbound
route to subscribe to, so one poller per page re-runs the server's reads on a timer and
skips the tick while the tab is hidden.

**A credential is never chosen by a caller.** The two proxy routes exist only because a
button in a client component cannot call the server's fetch directly. Each looks the
requested method and path up in an explicit allowlist **first**, and an unlisted path is
a 404 that no credential was ever attached to. Which authority a listed route uses is a
property of the row, not of the request.

[docs/web.md](web.md) has the screens, the probe, the allowlist, and why the CSP nonce is
load-bearing rather than hygiene.

## Trust boundaries

| Boundary | What is trusted |
|---|---|
| operator → goal engine | An operator key. Can move targets, resolve approvals, release the kill switch. |
| agent → goal engine | A bot key. Can state goals, read, report push values, ask to spend. Cannot grant itself any of the above. |
| goal engine → core | A **bot** key in core. One route: file a task. |
| core → goal engine | A **bot** key in the engine. Read the kill switch, ask to spend, report tokens. Core cannot approve its own spending, release the switch, or move a target. |
| goal engine → your databases | A read-only DSN, in a read-only transaction, with a timeout. |
| operator → YAML | Metric definitions and spending policies in the engine; tool grants, MCP servers and HTTP tools in core. No API writes any of them; a restart is required to change them, which is the point. |
| person → core | An account and a session. Scoped to their own chats, runs, workspace and token budget, in the `WHERE` clause. |
| person → console | A session cookie whose contents are sealed — the browser holds ciphertext, not core's token. Nothing in the page can read it and nothing outside the server can decrypt it. |
| console → core | The signed-in person's session token, forwarded as theirs. There is **no** core machine key in this app's environment, so the console cannot dispatch a task or act as anyone but the person at the keyboard. |
| console → goal engine | A bot key for reading and for engaging the switch. The three operations that need more — resolve, release, retarget — use a key the person pasted minutes ago. A call declared operator with no such key held is refused before it is sent, and a bot key offered as an operator key is refused at the paste, so it never becomes one. |
| browser → console | An allowlisted method and path, checked before a credential is chosen, on a request the browser's own `Sec-Fetch-Site` says came from this origin. Everything else is a 404 or a 403 that reached neither service. |
| model output → core | Nothing. A tool name the model asked for is looked up in the run's grant snapshot, and an unknown name is denied rather than passed through. The amount it fills into a spending argument is read and handed to the engine's gate, not believed. |
| agent-authored code → the host | Runs in a sandbox: `docker` gives it no network by default, no capabilities, a read-only root, memory and pid limits, and one workspace. `local` gives it this host — see below. |

Two rows deserve more than a table cell.

**`SANDBOX_BACKEND=local` is not a boundary.** It runs the generated script as the user
this service runs as, and its own source says so. It exists so a laptop with no docker
daemon can drive a run end to end. Never inside core's container, where a script would
run beside the provider API keys and the docker socket.

**`SANDBOX_BACKEND=docker` is a boundary, not a wall.** It is a shared kernel. And
reaching the host's daemon at all means core's container has the socket mounted, which
is root-equivalent access to the host — the compose file writes that down where it
happens. The roadmap moves to a dedicated sandbox provider. Until then, do not give core
credentials you would not give a contractor on their first day, and do not put anything
else on that box.

Prompt injection is bounded here, not prevented. That the agent can be talked into using
a tool you granted it is the reason the grants are narrow, the classes are the
operator's to declare, and spending leaves core entirely. That it could be talked *past*
a grant would be a vulnerability — see [SECURITY.md](../SECURITY.md).

## Things deliberately not built

In the goal engine:

- **No task queue.** The monitor is a timer over a bounded batch. A queue would
  add a moving part to the layer whose job is to be predictable.
- **No metric write API.** See above: definitions carry SQL.
- **No audit mutation path.** Not "an admin-only endpoint" — no method at all.
- **No agent-facing config.** Every limit an agent operates under is in a file the
  agent cannot reach.

In core:

- **No tool-grant API, and no `tool_grants` table.** Same rule, same reason.
- **No per-user spending policy.** The gate's safety comes from limits living in
  operator-owned YAML read once at startup. Per-user policies would have to live in a
  database users can edit, which destroys that; a single shared cap in core would mean
  one person's spend silently consuming another's. The person who owns the money owns
  the gate.
- **No run-step mutation path.** As with the audit log: no method at all.
- **No open registration by default**, and the first account is a CLI subcommand rather
  than a route that could be left on.
- **No per-vendor integration marketplace.** MCP plus operator-declared HTTP endpoints
  covers the same ground through two protocols instead of a directory of adapters.
- **No phone or SMS channel.** A [non-goal](motivation.md#non-goals) from the start,
  and it stays out until the autonomy layer has been boring for a while.

In the console:

- **No operator key in its environment.** A compromised console process can read, and it
  can engage the kill switch. It cannot release one, resolve an approval or move a
  target, because the credential for those is not there to steal.
- **No generic pass-through.** There is no proxy route that forwards whatever it is
  given. Making an upstream path reachable from a browser is a row added to an
  allowlist, beside a test asserting that only three of those rows may write anything.
- **No write path to any YAML**, so nothing on any screen can define a metric, grant a
  tool or set a spending limit — the same rule as the two services, from the layer a
  person actually clicks in.
- **No data of its own.** No database, no session store, no cache. Which means the whole
  of what it holds is two cookies, and both are sealed.
