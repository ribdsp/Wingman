# CLAUDE.md

Project-specific guidance for Claude Code working in this repository.

## What this is

Wingman: a self-hosted assistant that acts on a business without being asked. Two Go
services and a console, all in this repository, all tracked, all deployed separately.

| | |
|---|---|
| `goal-engine/` | Goals, metric monitoring, the trigger bridge, the spending gate, the audit log. Decides **whether** to act and **whether** to spend. |
| `core/` | The agent loop, tools, sandboxes, accounts, chats, the chat platforms it can be reached on, per-user budgets. Decides **how far** one run may go. |
| `web/` | The operator console. Decides nothing. Renders both services and forwards what an operator resolves. |

Two Go modules (`github.com/ribdsp/wingman/goal-engine`, `.../core`), two Postgres
databases, one Next.js app, one CI. They talk over HTTP: the engine dispatches a task to
core; core asks the engine's gate for permission to spend and reports its token use back;
the console reads both. Neither service is a library of the other, and there is no shared
module — the ~200 lines of response envelope are duplicated on purpose, because coupling
two independently deployed services costs more than the duplication. The console has no
database and no state of its own.

Read [docs/motivation.md](docs/motivation.md) for intent,
[docs/architecture.md](docs/architecture.md) for structure,
[docs/goal-engine.md](docs/goal-engine.md) and [docs/core.md](docs/core.md) for the two
decision models, [docs/web.md](docs/web.md) for the console's credential handling.

`PRD.md` may exist in the working tree. It is **gitignored** — it names products,
people and infrastructure that are not public. `docs/motivation.md` is its redacted
public equivalent and the one to keep in step with the code. Do not quote PRD.md
into a tracked file, and do not un-ignore it.

## Commands

The same set in either Go module — from `goal-engine/` or from `core/`:

```bash
go test ./...                    # the whole suite; no database, no network needed
go build ./...
gofmt -l ./cmd/ ./internal/      # must print nothing
go vet ./...
go test -coverprofile=coverage.out -covermode=atomic ./...
go tool cover -func=coverage.out | tail -1
```

A change to one module still needs the other's suite run: they share no code, but
`goal-engine/internal/core/client.go` and core's task route are a contract.

And in `web/`:

```bash
npm run typecheck                # tsc --noEmit, over the pages too
npm run test                     # vitest; every test is a pure function under lib/
npm run build                    # a second type check, and proves no route went static
```

`npm --prefix ./web run <script>` from the repository root, since the Bash cwd here has
a habit of not being where you think. All three run with both services down.

Each Go module has a Makefile, but **`make` is not installed on this machine** — read the
target and run the `go` line directly.

**Docker is usually not running here.** All three Dockerfiles and
`deploy/docker-compose.yml` are syntax-verified, not executed. Do not claim a
container works.

**`-race` needs cgo and gcc, which are absent here.** It runs in CI only. Do not
report a race-clean run you did not perform.

## Layering — enforced, not aspirational

Both Go modules have the same shape:

```
cmd/<service>          process: config, wiring, workers, shutdown
internal/handler       HTTP: bind, render, status codes          ← gin lives here
internal/middleware    request id, recover, access log, rate limit, auth
internal/service       orchestration, authorisation, audit writes
internal/repository    SQL. One type per table, parameterised
internal/database      pool, migrations
internal/domain        decisions. No I/O, no clock, no database
internal/utils         the response envelope
```

The goal engine adds `internal/metrics` (registry + samplers), `internal/policy`
(spending policy registry) and `internal/core` (client for core).

Core adds leaf packages that services reach through ports and that know nothing about
HTTP: `internal/provider` (Anthropic, OpenAI), `internal/tool` (registry, MCP,
operator-declared HTTP endpoints), `internal/sandbox` (local, docker),
`internal/agent` (the loop), `internal/auth` (argon2id, session tokens, link codes),
`internal/channel` (Telegram, Slack, Discord behind one port, plus the hub that owns
their lifetimes), `internal/goalengine` (client for the goal engine).

Three rules that a change must not break:

1. **`internal/domain` is pure.** Values in, values out. No `time.Now()` — `now` is
   a parameter. No database handle, no HTTP client, no logger. This is why the
   evaluator and the stop-reason ladder are both testable in milliseconds.
2. **Services depend on interfaces in `internal/service/ports.go`**, not on
   repositories. Most ports have one or two methods. Repositories satisfy them; so
   do the fakes in `fakes_test.go`.
3. **`Actor{Type, ID, RequestID}` is an explicit parameter.** Never smuggled through
   `context.Context`. Every audit row is written from it.

Nothing in `internal/service` imports `gin`. Nothing in `internal/handler` writes
SQL.

The console has a shape of its own, and one rule that matters as much as the three
above:

```
proxy.ts               the CSP, per request, around a fresh nonce
app/(console)/         every screen. Server components. The layout is the gate
app/signin/            outside the group, because the group's layout redirects here
app/api/               the two proxy handlers, session, operator authority
components/            presentation, and the client components that write
lib/upstream.ts        the ONLY module that makes an outbound request
lib/proxy-routes.ts    the allowlist: what a browser may ask to have forwarded
lib/session.ts         the two sealed cookies
lib/seal.ts            AES-256-GCM, purpose bound as AAD
lib/env.ts             every setting, validated all at once, refuses to boot
lib/{pace,thread,tone,format,search,envelope}.ts   pure functions. Where the tests are
```

4. **A credential is never chosen by a caller.** `lib/upstream.ts` picks it from the
   call's declared authority (`bot`, `operator`, `preferOperator`) and nothing else. A
   page cannot spend operator authority on a read, and `verifyOperatorKey` is the one
   function that takes a key as an argument — deliberately, so the generic path never
   does.

Reads happen in server components through `lib/upstream.ts` directly. A route in
`lib/proxy-routes.ts` exists only because a *client* component needs it. If you are
adding a row so a page can read something, the read is in the wrong place.

## Conventions

- **API JSON is camelCase. `db:` tags are snake_case.** Map at the repository
  boundary; snake_case never reaches the public API.
- **Shared envelope** for every response (`success/code/message/data/error/meta`) via
  `internal/utils`, duplicated in both modules. Do not invent a second shape. The console
  parses that one shape in `web/lib/envelope.ts` and renders the message as written.
- **Nine stable error codes, shared by both services**, and reused by the console for its
  own refusals. Listed in [docs/api.md](docs/api.md#errors). Adding a tenth is an API
  change, not a detail — neither core nor the console needed one.
- Files 200–400 lines, 800 max. Functions under 50 lines. Early returns over
  nesting.
- Comments explain **why**. The line explaining why a cap denies instead of
  escalating is worth more than any `// bind the request`.
- Immutable by default — an evaluation is an audit record, build a new one.
- Errors wrapped with context, never swallowed. A 500 body carries no driver
  message, DSN, query or host name.

## Safety invariants — do not "simplify" these

Each is enforced in code and documented with its reasoning. Changing one needs an
argument in the PR, not a diff.

In the goal engine:

- Privilege is fixed at startup by which env var listed the key
  (`GOAL_ENGINE_API_KEYS` = operator, `GOAL_ENGINE_BOT_KEYS` = bot). No request field
  can raise a role.
- Operator-only operations are checked **twice**: route middleware *and* the service.
  This is not redundancy to clean up.
- `PUT /v1/flags/kill-switch` is deliberately **not** route-gated — anyone may
  engage, only an operator may release, enforced in the service. Do not add
  `RequireOperator` to that route.
- An unreadable kill-switch flag means **engaged**. Unreadable means halt.
- A missing spending policy means **`pending`** (ask a human), never auto-approve.
  `config/policies.yaml` ships empty on purpose.
- Caps (`hardCap`, `dailyCap`) **deny**; they do not escalate to a human.
- `enabled: false` **denies**.
- Metric definitions exist only in operator-owned YAML. **Never add an API that
  creates one** — they carry SQL and the names of env vars holding DB passwords.
- The audit log has **no** update or delete method. Not "admin-only" — none.
- Goal targets are operator-only to edit. An agent must not be able to lower the
  bar.
- Trigger cooldown and per-period budget have restrictive defaults and hard floors.
  Omitting them must never mean zero, because zero means "no limit" to the
  evaluator.
- The evaluation ladder's **order** is the safety model. Ten branches, tested one
  per branch. Reordering is a behaviour change.
- Sample staleness (`METRIC_MAX_SAMPLE_AGE`) stops evaluation rather than reading a
  dead feed's last number as on-track.
- A notification carries **no figure** — no amount, no currency, no observed value, no
  pace ratio, and a test asserts the headline holds no digit. It says what happened, the
  id, and a console link. It is emitted only after the decision it describes is durable,
  is never retried, and must never be able to affect one: a failure is logged and
  dropped, exactly like `appendAudit`. `NOTIFY_ENABLED` is false by default.
- Secrets are never logged, echoed or printed: not API keys, not DSNs (only
  datasource names), not the core API key.

In core, the same shape of rule for a different decision:

- Three caller kinds, fixed at startup by where the credential lives: `CORE_API_KEYS`
  = operator, `CORE_BOT_KEYS` = bot, a row in `users` plus a session = a person. An
  environment key is never a user; a user session can never dispatch a system task.
  No request field moves anyone between them.
- Operator-only operations are checked **twice** here too.
- **The stop-reason ladder's order is the safety model.** Eleven reasons, one test per
  branch. `halted` and `completed` must never be produced by the same path — "I could
  not tell" is not "it is fine".
- **The tool ladder's order** likewise. Eight rows, and a tool with no grant in
  `config/tools.yaml` is **denied**, not asked about.
- Tool grants, MCP servers and HTTP tool declarations exist only in operator-owned
  YAML — `config/tools.yaml`, `config/mcp.yaml`, `config/http-tools.yaml`. **Never add
  an API that writes one.** All three ship declaring nothing.
- A run's tool set is snapshotted when the run starts and is immutable for its
  lifetime. A grant edited mid-run does not widen a run already going.
- Money is never decided in core. A spending tool with a policy contract files a
  request with the engine's ladder; one **without** a contract is denied. There is no
  second, weaker gate here.
- Every run limit has a restrictive default and a hard floor: iterations, tool calls,
  tokens per run, tokens per person per day, the step timeout, the sandbox timeout.
  Zero means unset, never "no limit". `USER_DAILY_TOKEN_CAP=-1` is the only way to
  remove a cap and it has to be written out.
- A run that cannot read its own spend ledger **stops** (`budget_unreadable`). No
  decision is made against a broken ledger.
- The kill switch is checked before a run starts and between iterations, fail-closed.
- A write tool on an unattended run is **refused for that run**, not queued.
- `run_steps` is append-only. Same rule as the audit log: no update, no delete.
- Registration is closed by default. `core createuser` reads the password from
  **stdin**, never a flag — a flag is in the process list and in shell history.
- Every query over user content carries `user_id` in the `WHERE` clause, and a
  workspace belongs to one person.
- `SANDBOX_BACKEND=local` is not isolation and the code says so. Never inside core's
  own container: a generated script would run beside the API keys and the docker
  socket.
- The sandbox gets **no network** unless an operator grants one.
- Every channel connection is **outbound** — Telegram long polling, Slack Socket Mode,
  the Discord gateway. There is no inbound webhook route and no signature to verify,
  because nothing is listening. Do not add one.
- **The inbound ladder's order is the safety model.** Seven rungs then accept, one test
  per rung. Rung 4 (is this sender linked?) settles before rung 5 (throttle), so an
  unlinked sender reaches exactly one thing: the instruction to link.
- A chat identity is attached to an account **only** by redeeming a single-use code sent
  over the channel — the message is the proof. No API attaches an external id. Codes are
  stored hashed, shown once, minutes long, and unknown, expired and spent are one answer
  so the sender cannot tell which they hit.
- `CHANNEL_ALLOW_GROUPS` is **false** by default and warns at every startup when on. In
  a shared room the linked person's budget is spendable by anyone who can type.
- A channel message takes the same path into the agent a trigger does, and is bounded by
  the same run limits. There is no channel-specific route around them.
- A notification's recipient is the `CORE_UNATTENDED_OWNER` account's own linked chat
  identities, resolved from the sender's external **user** id and never from a room. The
  request carries **no recipient field** — never add one — and a signed-in person is
  refused, so an account on the box cannot make core message the operator. Revoked links
  are skipped, one platform failing does not cost the others their message, and there is
  no history route: `GET /v1/notifications` is a `405`.
- Secrets are never logged, echoed or printed — including that a tool config holds
  variable *names* and a value that looks like a secret is refused at startup, that
  adapter errors are redacted before they reach a log, and that the goal-engine
  credential is asserted by test never to appear in an error.

In the console, whose only job is to hold credentials without leaking them:

- The console's own credential is a goal-engine **bot** key. An operator key is never in
  its environment: it is pasted, verified by a side-effect-free probe, sealed into that
  browser's cookie for `WEB_OPERATOR_TTL_MINUTES` (30 default, 480 ceiling, no renewal)
  and shown as a countdown. **Do not add `GOAL_ENGINE_OPERATOR_KEY` to this app.**
- There is **no core machine key** here at all. Every core call is the signed-in person's
  own session, which is why the console cannot dispatch a task or administer accounts.
- Both cookies are sealed (AES-256-GCM), `httpOnly`, `SameSite=Strict`, with the purpose
  bound as AAD so one cannot be replayed as the other, and the expiry inside the sealed
  bytes. There is no third cookie — **never** a `role` or `isOperator` one.
- `lib/proxy-routes.ts` is an allowlist and the unlisted path is refused **before any
  credential is chosen**. Adding a row is a decision. The absent ones are absent for
  reasons written next to them — metric sample writes, `POST /v1/monitor/tick`, task
  dispatch, account administration, `POST /v1/notifications`, and the three core routes
  that would kill the console's own session.
- Query parameters are allowlisted by name *and* pattern. An unknown one is dropped.
- Panels fail soft, **except the kill switch, which fails closed**: unreadable reads as
  engaged, the same rule as in both services.
- **The CSP carries a per-request nonce and cannot be static.** A static `script-src
  'self'` blocks Next's streamed inline scripts, hydration fails with React #412, and
  every button on a page that looks perfect is dead. If a script is blocked, the answer
  is a nonce, not `'unsafe-inline'` — this origin has buttons that resolve spending
  decisions. `lib/csp.test.ts` guards both halves.
- Every read happens on the server. No key, no token and no service base URL reaches the
  browser; `publicConfig()` exists so nothing is tempted to import `config()` into a
  client component.

## Tests

Bar is 80%, whole-module. The goal engine is at **93.8%**; core is at **86.6%**
whole-module, **92.4%** excluding `cmd/core`, and decision code in both is higher.
`cmd/core` sits at 16.1% because it is wiring, and wiring is proved by the build —
what is tested there is the handful of things in it that are not wiring.

Write the test first — for this codebase specifically, the only way to know a change
did not reorder the engine's ten-branch evaluation ladder, core's eleven-reason stop
ladder or its seven-rung inbound ladder is a test per branch that existed beforehand.

Arrange–Act–Assert. Name the behaviour, not the function:

```go
func TestEvaluate_behindPaceInsideCooldown_doesNotTrigger(t *testing.T)
```

Repositories use `go-sqlmock`; services use the fakes in `fakes_test.go`; providers are
tested against `httptest` servers. **Do not add a test that needs a live Postgres to
either unit suite** — integration tests belong behind a build tag (roadmap item 2).

The console is measured differently, on purpose. It owns no schema and makes no
decisions, so there is no coverage bar over it: what a suite can prove there is that the
pure functions under `lib/` are right — the proxy allowlist, the envelope parser, the
sealing, the pace and thread maths, the formatters, the tone table, the CSP. **229 tests
in 10 files**, `vitest`, `environment: 'node'`. No jsdom and no rendering test: a
component that only arranges what a server component already fetched has nothing to
assert that `tsc` does not. `npm run typecheck` and `npm run build` are the other two
gates, and the build is also the proof that no route accidentally went static.

## Docs must match the code

Every claim in `docs/` was verified against source. If you change behaviour, update
the doc in the same commit. The files that make specific claims:

| | Claims |
|---|---|
| `docs/api.md` | Every route on both services, default, bound, error code, the approval ladder |
| `docs/goal-engine.md` | Pace maths, the ten-branch ladder, the approval ladder, when a notification is emitted |
| `docs/core.md` | The agent loop, the eleven stop reasons, the tool ladder, run limits, the sandbox flags, the seven-rung inbound ladder, how linking works, who a notification reaches |
| `docs/web.md` | The console's screens, its three credentials, the operator probe, the proxy allowlist and what is absent from it, the CSP nonce, its settings |
| `docs/architecture.md` | Layering, the task payload shape, table lists |
| `docs/deployment.md` | Env vars, compose behaviour, troubleshooting |
| `goal-engine/config/policies.yaml` | The approval ladder, in comments an operator reads |
| `core/config/tools.yaml` | The tool ladder and what a grant means, likewise |
| `web/.env.example` | The console's full env surface, with defaults, floors and the ceiling |

A doc and the code disagreeing is a bug in one of them.

## Do not

- Clone research repositories into this tree. Use TEMP.
- Add a dependency without a reason that survives "could this be 20 lines?". This holds in
  `web/` too: no schema library, no ESLint, no UI kit for one flourish. The console's
  dependencies are Next, React, Tailwind and the test runner.
- Commit or push unless asked.
- Create or push a GitHub repository, or run `gh repo fork`, without explicit
  approval — publishing is outward-facing.
- Put secrets in any `.env.example` beyond `CHANGE_ME` placeholders.
- Claim something was tested that was not. Docker, `make` and `-race` are all
  unavailable locally.

## Repo layout

```
README.md                  what it is, quick start, safety summary
docs/                      motivation, architecture, goal-engine, core, web, api,
                           deployment, roadmap
deploy/docker-compose.yml  both services, both databases, the console
deploy/.env.example        the stack's own settings; not handed to a container
goal-engine/               goals, metrics, triggers, the spending gate, audit
  cmd/ internal/ migrations/ config/
  .env.example             this service's full env surface
core/                      the agent loop, tools, sandboxes, accounts, chats,
                           Telegram/Slack/Discord
  cmd/ internal/ migrations/ config/
  .env.example             this service's full env surface
web/                       the operator console: deck, goals, audit, chat, authority
  proxy.ts app/ components/ lib/
  .env.example             this app's full env surface
PRD.md                     gitignored; the private, unredacted requirements
core/workspaces/           gitignored; whatever an agent wrote while working
web/node_modules/ .next/   gitignored
```

## Language

**Everything in this repository is English.** Not only identifiers and commit messages —
code comments, test names, *test fixture values*, documentation prose, the shipped YAML
comments, and the example goals and briefs in `README.md` and `docs/`. Example goal text
written in the author's own language demonstrated the feature perfectly and made the
project unreadable to most of the people it is published for; it is gone, and a grep for
non-English vocabulary should keep returning nothing.

The same rule covers locale defaults, which are a subtler version of the same mistake:
`TIMEZONE` defaults to `UTC` in both services, because a self-hosted service that
silently renders every timestamp — and, in core, places every daily token cap boundary —
in whichever zone the author happened to work in is wrong for everyone else. Test
fixtures that need a *non-UTC* zone to prove the plumbing works are a different thing and
stay: their point is the offset, not the place.

Two deliberate exceptions, both about what is *not* published: the private `PRD.md` is
Indonesian and stays that way, and a conversation with the user is held in whichever
language they write in. Neither reaches a tracked file. `docs/motivation.md` is the
public, English equivalent of `PRD.md`.
