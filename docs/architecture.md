# Architecture

Wingman is two services that share nothing but an HTTP boundary.

```
                      ┌──────────────────────────────────────────┐
   you ──────────────▶│  core  (fork of Rakazo, TypeScript)      │
   chat, tasks        │  agents · channels · tools · sandbox     │
                      └──────────────────────────────────────────┘
                                      ▲
                                      │  POST /api/tasks
                                      │  "MRR is 12% behind pace"
                                      │
                      ┌───────────────┴──────────────────────────┐
   you ──────────────▶│  goal-engine  (this repo, Go)            │
   goals, approvals   │  goals · monitor · trigger bridge        │
                      │  approval gate · audit log              │
                      └──────────────────────────────────────────┘
                            │                    │
                    ┌───────▼──────┐     ┌───────▼─────────────┐
                    │ own Postgres │     │ your product DBs    │
                    │ (read/write) │     │ and APIs (read only)│
                    └──────────────┘     └─────────────────────┘
```

## Why two services

Core is a fork. Forks are only maintainable if the thing you added is not tangled
into the thing you are pulling from upstream. Every feature this project adds —
goals, metric monitoring, the spending gate, the audit trail — lives outside core,
so `git pull upstream` is a routine operation rather than a merge negotiation.

The rest of the reasons follow from that one:

- **Separate database.** The audit log and the approval queue must survive a core
  upgrade, a core rollback, and a core `prisma migrate reset`. They are not in
  core's schema.
- **Separate language.** Go for the part that has to be boring: a single static
  binary, no runtime, no dependency tree to audit before letting it hold the
  brakes.
- **Separate blast radius.** Core runs agent-authored code in a sandbox. The
  service that decides whether money may move should not be in that process.
- **Separate uptime.** If core is down, the goal engine keeps evaluating and
  keeps recording; the dispatch fails and is retried. If the goal engine is down,
  core still answers you — it simply stops being woken.

## What crosses the boundary

Exactly one call, in one direction: the goal engine creates a task in core.

```
POST {WINGMAN_CORE_BASE_URL}{WINGMAN_CORE_TASK_PATH}      default: /api/v1/tasks
Authorization: Bearer {WINGMAN_CORE_API_KEY}

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

`idempotencyKey` lets core reject a duplicate when a response is lost in transit and
this service retries.

`WINGMAN_CORE_TASK_PATH` is configurable because core is a fork: an operator who
renamed the route should not have to fork this service too. `TRIGGER_DRY_RUN=true`
logs and records the task instead of sending it, which is how a new deployment is
meant to spend its first week.

Core does not call the goal engine. If you want an agent to state a goal or ask for
a spend, give it a **bot key** and let it call the API like any other client — the
same routes, the same limits, and the same audit trail as everything else.

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

## Trust boundaries

| Boundary | What is trusted |
|---|---|
| operator → API | An operator key. Can move targets, resolve approvals, release the kill switch. |
| agent → API | A bot key. Can state goals, read, report push values, ask to spend. Cannot grant itself any of the above. |
| goal engine → core | An API key core issued. One route, one direction. |
| goal engine → your databases | A read-only DSN, in a read-only transaction, with a timeout. |
| operator → YAML | Metric definitions and spending policies. No API writes them; a restart is required to change them, which is the point. |
| agent-authored code | Runs in core's sandbox, not here. |

The last row is where this project's largest remaining risk lives. Phase 1 uses
Docker isolation, which is a boundary, not a wall; the roadmap moves to a dedicated
sandbox provider. Until then, do not give core credentials you would not give a
contractor on their first day.

## Things deliberately not built

- **No task queue.** The monitor is a timer over a bounded batch. A queue would
  add a moving part to the layer whose job is to be predictable.
- **No metric write API.** See above: definitions carry SQL.
- **No audit mutation path.** Not "an admin-only endpoint" — no method at all.
- **No agent-facing config.** Every limit an agent operates under is in a file the
  agent cannot reach.
- **No phone or SMS channel.** A [non-goal](motivation.md#non-goals) from the start,
  and it stays out until the autonomy layer has been boring for a while.
