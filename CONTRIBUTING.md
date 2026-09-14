# Contributing

Thank you for looking. A few things worth knowing before you spend an evening on a
patch.

- [What this project wants](#what-this-project-wants)
- [Getting set up](#getting-set-up)
- [The layering rules](#the-layering-rules)
- [Tests](#tests)
- [Style](#style)
- [Commits and pull requests](#commits-and-pull-requests)
- [Review](#review)
- [Changing a safety default](#changing-a-safety-default)
- [Security issues](#security-issues)

## What this project wants

Wingman's job is to let an agent act on a business without being asked, and to make
the ways that goes wrong survivable. The interesting constraints are all in the
second half of that sentence.

Most valuable contributions, roughly in order:

1. **A pilot report.** You ran it against a real metric for a month; here is what
   the decisions looked like and where the defaults were wrong. This is worth more
   than any patch, and [docs/roadmap.md](docs/roadmap.md) explains why.
2. **Integration tests against a real Postgres.** The repositories are tested
   against a mock driver, which proves the SQL is shaped right, not that it is
   right.
3. **Bug reports with a failing test.** Anything in either service's
   `internal/domain` can be reproduced in a unit test with no database at all, so a
   report there can be exact.
4. **Documentation that corrects the code's behaviour.** If a doc and the source
   disagree, that is a bug in one of them and finding it is a real contribution.

Less useful: new features in the goal engine. It is deliberately small, and
[the absent list](docs/roadmap.md#deliberately-absent) is a set of decisions rather
than a backlog. Core is younger and has more room, but the same question applies to
a new tool source or a new provider: what does it let an operator do that they
cannot do today, and what does it let a wrong agent do?

## Getting set up

Two Go modules and one Node app, each with its own manifest, each deployed separately:

| | |
|---|---|
| `goal-engine/` | goals, metrics, the trigger bridge, the spending gate, the audit log |
| `core/` | the agent loop, tools, sandboxes, accounts, chats |
| `web/` | the operator console. Decides nothing; renders both services |

```bash
git clone https://github.com/ribdsp/wingman.git
cd wingman/goal-engine && go mod download && make test
cd ../core            && go mod download && make test
cd ../web             && npm ci && npm run test
```

Go 1.26 or newer, Node 22 or newer. Nothing else is required for the tests — no
database, no Docker, no network, and for core no provider API key. That is on purpose:
a test suite you can run on a plane gets run.

To run either service you need a Postgres, and they want **separate** ones — that
separation is the point of the compose file:

```bash
cp .env.example .env
$EDITOR .env                # DATABASE_URL, and the keys for that service
make run
```

Core also needs its first account, because registration is closed by default:

```bash
read -rs PW && printf '%s' "$PW" | make createuser EMAIL=you@example.com NAME="Your Name"
```

`make` with no target lists everything. Both modules have the same targets:

| | |
|---|---|
| `make test` | The suite. |
| `make test-race` | Under the race detector. Needs cgo and a C toolchain; CI runs it on every push, so nothing merges unraced. |
| `make cover` | Coverage, with the total printed. |
| `make fmt vet lint` | Formatting, vet, and golangci-lint. |
| `make build` | A stamped static binary into `bin/`. |

CI runs golangci-lint at a pinned version and its findings gate the merge, so `lint`
is not advisory. The target skips silently when the linter is absent, and — the trap
this pipeline itself fell into for a while — a golangci-lint built against an older Go
than the module's `go` directive exits without having linted anything. If you install
it, install it with a current toolchain:

```bash
GOTOOLCHAIN=latest go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.13.2
```

Neither module has a `.golangci.yml`: the standard linter set passes as written, and
an exclusion nobody needs is an exclusion nobody notices growing.

On Windows, `make` is often absent and every target is one `go` command — read the
Makefile and run the line you need.

The console has no Makefile and needs no database of its own:

```bash
cd web
cp .env.example .env.local        # both base URLs, a bot key, a 32-byte cookie secret
npm run typecheck                 # tsc --noEmit, over the pages too
npm run test                      # vitest, lib/**/*.test.ts
npm run build                     # a second type check, and proves no route went static
npm run dev
```

It builds and type-checks with both services down. It cannot be *usefully* run that
way: every panel would render an unreachable note and the strip would read the kill
switch as engaged, which is correct behaviour and not a state to develop against.

## The layering rules

Both modules have the same shape, and a patch that breaks either of the two rules
below will be sent back:

**`internal/domain` is pure.** In the goal engine it decides whether a goal is behind
pace, what a spend request gets, and what a brief says. In core it decides why a run
stopped, whether a tool may be called, and what a budget allows. It takes values and
returns values: no database handle, no `time.Now()`, no HTTP client, no logger. `now`
is a parameter.

This is why the evaluator can be tested against two dozen hand-written scenarios in
milliseconds, and why "what would this goal do tomorrow" is a function call. If you
find yourself wanting a clock in there, pass the time in.

**Services own the I/O, behind small ports.** Every dependency a service has is an
interface in `internal/service/ports.go` — `GoalStore`, `SampleReader`, `AuditSink`,
`MetricLookup`, `TaskCreator`, `Notifier`, `Clock` in the engine; `RunStore`,
`RunAuditor`, `SpendReader`, `TaskQueue`, `AgentRunner`, `ToolSource`,
`ChannelDirectory`, `DirectSender` and friends in core — mostly one or two methods
each. The repositories satisfy them, and so do the fakes in the tests.

Small on purpose, and split where the split is the safety property: core's notifier
takes `ChannelDirectory`, which can only *list* a person's linked chats, rather than the
`ChannelLinks` port next to it, which can also revoke one. The path that sends a message
has no business taking a connection away, and the same reasoning separates
`SessionRevoker` from `SessionStore`.

```
cmd/<service>          process: config, wiring, workers, shutdown
internal/handler       HTTP: bind, render, status codes
internal/middleware    request id, recover, access log, rate limit, auth
internal/service       orchestration, authorisation, audit writes
internal/repository    SQL. One type per table, parameterised
internal/domain        decisions. No I/O
```

Core adds leaf packages that the services depend on through ports and that know
nothing about HTTP: `internal/provider` (Anthropic, OpenAI), `internal/tool` (the
registry, MCP, declared HTTP endpoints), `internal/sandbox` (local, docker),
`internal/agent` (the loop), `internal/auth`, `internal/channel` (Telegram, Slack,
Discord behind one port), `internal/goalengine`.

Nothing in `internal/service` imports `gin`. Nothing in `internal/handler` writes
SQL. If your change needs to break that, say why in the pull request — it is a
design conversation, not a lint rule.

Two more, smaller:

- **`Actor` is an explicit parameter**, never smuggled through `context.Context`.
  `{Type, ID, RequestID}` is passed down from the handler, because a caller's
  identity is an argument to a decision and every audit row is written from it.
- **Errors go up, not into a log-and-continue.** The one exception is the audit
  write, which logs loudly rather than rolling back the action it was recording;
  that trade-off is documented where it happens.

The console has one rule of its own, and it is the same idea in a different language:
**a credential is never chosen by a caller, and never reaches a browser.** Every read
happens in the Node process; `lib/upstream.ts` decides which credential a call gets
from the authority the call needs; `lib/proxy-routes.ts` is an allow-list of the routes
a client component may reach, with the write operations named one by one. A patch that
adds a generic pass-through, moves a read into the browser, or puts an operator key in
the console's environment will be sent back — [docs/web.md](docs/web.md) says why for
each. If a page needs something new, the read belongs in the server component, not in a
new proxy row.

## Tests

The bar is 80%. Both suites clear it, and in both the packages that make decisions
are highest:

```
goal-engine 93.8% total          core 86.6% total (92.4% excluding cmd)

internal/middleware   100.0%     internal/utils        100.0%
internal/utils        100.0%     internal/provider      99.6%
internal/domain        98.7%     internal/tool          99.0%
internal/service       98.4%     internal/middleware    98.3%
internal/config        97.0%     internal/domain        97.6%
internal/policy        95.7%     internal/config        96.9%
internal/core          95.7%     internal/goalengine    96.2%
internal/handler       90.5%     internal/agent         94.1%
internal/metrics       88.6%     internal/sandbox       93.9%
internal/repository    88.2%     internal/service       90.7%
                                 internal/repository    88.9%
                                 internal/auth          85.7%
                                 internal/handler       84.8%
                                 internal/channel       83.7%
                                 internal/database      78.4%
                                 cmd/core               16.1%
```

`cmd/core` is low because it is wiring, and wiring is proved by the build. What is
tested there is the handful of things in it that are not wiring: how long a run may
be in flight before the sweep calls its worker dead, what a spending request tells
whoever has to approve it, and how a password reaches the account it creates.

Write the test first. Not as ceremony — for this codebase specifically, the goal
engine's evaluator has ten branches and core's stop-reason ladder has eleven, whose
*order* is the safety model in both. The only way to be sure a change did not reorder
them is a test per branch that existed before the change.

Arrange–Act–Assert, and name the behaviour rather than the function:

```go
func TestEvaluate_behindPaceInsideCooldown_doesNotTrigger(t *testing.T) {
    // Arrange
    goal := activeGoal(t, withCooldown(6*time.Hour), withLastTrigger(now.Add(-2*time.Hour)))
    sample := sampleAt(now, 36_000_000)

    // Act
    got := domain.Evaluate(goal, sample, now)

    // Assert
    if got.Decision != domain.DecisionCooldownSkipped {
        t.Fatalf("decision = %s, want cooldown_skipped", got.Decision)
    }
}
```

Table-driven where the cases are genuinely uniform, separate functions where each
case needs its own setup. Repositories are tested with `go-sqlmock`; services with
the fakes in `fakes_test.go`. Do not add a test that needs a live Postgres to the
unit suite — that is item 2 on the roadmap and it belongs behind a build tag.

The console's suite is 229 tests over the pure modules under `web/lib`: the sealing,
the route allow-list, the envelope parser, the pace and thread maths, the formatters,
the tone table, the CSP. No coverage figure is claimed for it, and there are no browser
tests: a component that only arranges what a server component already fetched has
nothing to assert that `tsc` does not. Two of those files are load-bearing in the same
way the Go ladders are — `proxy-routes.test.ts` asserts which routes may write and at
what authority, and `csp.test.ts` asserts the nonce reaches `script-src` and that
`script-src` never acquires `'unsafe-inline'`. Change either behaviour and the test is
where you say so.

## Style

`gofmt` decides formatting; there is nothing to discuss. Beyond that, this codebase
has some habits:

- **Files stay small.** 200–400 lines is typical, 800 is the ceiling. Split by what
  the code does, not by what type it is.
- **Functions stay small.** Under 50 lines, early returns rather than nesting.
- **Comments say why, not what.** `// bind the request` is noise. The comment
  explaining why a cap denies instead of escalating is the most valuable line in
  that file.
- **JSON is camelCase, database tags are snake_case.** Mapping happens at the
  repository boundary, and snake_case never reaches the public API:

  ```go
  IsActive  bool      `db:"is_active" json:"isActive"`
  CreatedAt time.Time `db:"created_at" json:"createdAt"`
  ```

- **No magic numbers.** Named constants for thresholds, timeouts and limits.
- **Immutable by default.** An evaluation is an audit record; build a new one rather
  than mutating one in place.
- **Errors are wrapped with context and never swallowed.** A 500 body carries no
  driver message, DSN, query or host name — `requestId` plus the service log is the
  way in.

In the console the same habits hold, in TypeScript: files stay small, comments say
why, `any` does not appear, and camelCase JSON is consumed exactly as the services
return it — no transform layer, because the two services already agree on the shape.
One deliberate departure from the usual TypeScript advice: there is no schema library.
`lib/envelope.ts` narrows the envelope by hand in one file, and it says why — the
console owns no schema, and the dependency rule ("could this be 20 lines?") applies
here too.

## Commits and pull requests

Conventional commits:

```
feat: add per-bot trigger budget
fix: reject a pushed sample for a sql metric
docs: correct the approval ladder in api.md
test: cover a zero-length goal period
refactor: extract the pace calculation
chore: pin the postgres image to 17-alpine
```

One logical change per commit. A pull request should say what it changes, why, and
what you ran. If it touches a decision path, say which test proves the order is
unchanged.

Before you open one:

```bash
make fmt vet test                  # in whichever module you touched
npm run typecheck && npm run test && npm run build     # in web/, if you touched it
```

`make test-race` too, if you have a C toolchain. CI runs it either way, and it covers
both modules, the console, and the three images.

## Review

Every change gets read. Findings come back at four levels:

| | |
|---|---|
| **CRITICAL** | A security hole or data-loss risk. Blocks the merge. |
| **HIGH** | A bug, or something that will become one. Fix before merge. |
| **MEDIUM** | Maintainability. Fix if you can. |
| **LOW** | Style, taste, a suggestion. Yours to take or leave. |

Anything touching authentication, authorisation, the approval ladder, the tool
ladder, the sandbox boundary, the kill switch, the audit log, SQL construction, or the
console's cookie sealing, route allow-list and CSP gets a security-focused pass as
well. That is not a comment on the contributor; it is where the consequences are.

## Changing a safety default

Some values in this codebase are arguments that have already been had. Changing one
is welcome, and it needs the argument rather than a diff.

In the goal engine:

- The default cooldown (6h) and its floor (15m)
- The default trigger budget (5 per period)
- The tolerance default (0.05) and its ceiling (0.5)
- Sample staleness (`METRIC_MAX_SAMPLE_AGE`, 26h)
- Approval TTL (24h)
- **The order of the evaluation ladder**
- **The order of the approval ladder**
- That a missing policy means "ask a human"
- That caps deny rather than escalate
- That anyone may engage the kill switch and only an operator may release it
- That an unreadable kill-switch flag means halt
- That notifications are off by default (`NOTIFY_ENABLED=false`)
- **That a notification carries no amount, no currency and no pace figure**, and that a
  failed one is logged and dropped rather than retried

In core:

- The run limits and their bounds: 15 iterations (1–200), 40 tool calls (1–500),
  250 000 tokens per run (1 000–10 000 000), 2 000 000 per user per day, a 3-minute
  step timeout (5s–15m), a 90-second sandbox timeout (1s–30m)
- That an unset limit means the default, never "no limit"
- **The order of the stop-reason ladder**
- **The order of the tool ladder**
- That a tool with no grant in `config/tools.yaml` is denied
- That a write tool is refused on an unattended run rather than queued
- That a spending tool with no policy contract is denied, and one with a contract
  goes to the goal engine's gate rather than a second gate in core
- That registration is closed by default
- That the sandbox gets no network unless an operator gives it one
- That a notification's recipient is the `CORE_UNATTENDED_OWNER` account's own linked
  chats, with no recipient field in the request and no route to the history

In the console:

- That an operator key is never in its environment, and is pasted, verified and held
  in a sealed cookie instead
- The operator authority TTL (30 minutes, floor 1, ceiling 480) and that it does not
  renew
- That the forwarded routes are an allow-list with the write operations named, rather
  than a pass-through
- That an unreadable kill switch puts the whole console in its halted state
- That a failed read degrades one panel instead of failing the screen

For the ladders and the deny-by-default rules, "it is inconvenient" is not an
argument — inconvenience is what they are for. What would move them is a failure mode
they cause that is worse than the one they prevent. If you have one, that is a
genuinely interesting issue and it should be opened as one.

Everything above is documented in [docs/goal-engine.md](docs/goal-engine.md),
[docs/core.md](docs/core.md) and [docs/web.md](docs/web.md) with the reasoning
attached. If you disagree with the reasoning, argue with that paragraph; it is there to
be argued with.

## Security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).

## Licence

Contributions are under Apache-2.0, the same as the project. There is no CLA. Adapted
source is allowed and has to be declared: `NOTICE` lists what the console takes from
Rakazo, which files it lives in and what was changed, because Apache-2.0 asks for
exactly that. If a change brings in more — from Rakazo or anywhere else — the pull
request says what, from where, and under what licence, and `NOTICE` grows with it. The
two Go services contain none and that is worth keeping true.

By opening a pull request you confirm you wrote the change or have the right to
submit it, and that it may be released under Apache-2.0.
