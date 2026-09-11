# Roadmap

What exists, what is next, and what is deliberately absent. Written to be honest
rather than optimistic — this project's whole argument is about restraint, and a
roadmap that promises everything would undercut it.

No dates. This is one person's project plus whoever shows up.

## Where it is now

| | |
|---|---|
| **goal-engine** | Built and unit-tested. The decision core is pure and covered by hand-written scenarios; the HTTP layer, repositories and workers are wired and boot cleanly. Never run in production. |
| **core** | Not written here. A fork of Rakazo you provision yourself, plus one route to accept a task. |
| **The two together** | Specified, dry-run-ready, and never exercised end-to-end by the author against a live core. |

Read that last row twice before you plan around this. `TRIGGER_DRY_RUN=true` is the
default because it is also the only mode anybody has spent real time in.

### Done

- Goal registry: targets, periods, baselines, tolerances, `gte`/`lte`.
- Pace evaluation: a goal is judged against where it should be *by now*, with a
  ten-branch decision ladder that records every outcome including the inert ones.
- Metric monitor: `push`, `sql` (read-only transaction, timeout-bounded) and `http`
  sources, declared only in operator-owned YAML.
- Trigger bridge: one authenticated call into core, idempotency-keyed, dry-runnable.
- Rate limits on autonomy: per-goal cooldown and a trigger budget per period, both
  with restrictive defaults and floors.
- Approval gate: deny-biased ladder, auto-approve threshold, daily and hard caps
  that refuse rather than escalate, TTL expiry swept on a timer and at startup.
- Kill switch: anyone engages, only an operator releases, unreadable means halt.
- Audit log: append-only, no service write path other than insert.
- Two privilege levels fixed by which environment variable held the key.
- Packaging: distroless image with a self-probing healthcheck, compose stack,
  migrations on boot.

### Phase 1 — partly

Deploying core on a VPS and validating chat, coding and its built-in approvals is
yours to do; this repo gives you the script, the compose stack and the rebrand
checklist. The basic UI rebrand is documented in
[fork-and-rebrand.md](fork-and-rebrand.md) but not performed — it cannot be, from
here, without a fork to perform it on.

### Phase 2 — built, unproven

The goal-driven layer exists as software. What has not happened is the end-to-end
run [the requirements](motivation.md#phases) ask for: a goal stated in chat, a gap
detected, a plan drafted, an approval granted, an action executed. Every piece of
that path is implemented on this side of the boundary. Nobody has watched the whole
thing happen.

That is the next milestone, and it is not a coding task. It is a pilot.

## Next

Ordered by whether the thing after it can be trusted without it.

### 1. One pilot product, end to end

One real goal, one real metric, one month. Not a demo — a goal somebody actually
cares about, watched in dry run until its decisions look like judgements a human
would have made, then let off the leash.

What this will find, and no amount of unit testing will:

- Whether tolerance defaults are sane against real, noisy metrics.
- Whether a 6h cooldown and 5 triggers per period is the right shape of attention
  or a source of nagging.
- Whether a brief built from `sourceText` plus numbers is actually enough for an
  agent to do useful work.
- How often the answer is "the goal was badly stated", which is the failure mode a
  monitor cannot fix.

Nothing below this line should be built before this happens. Adding features to an
autonomy layer nobody has lived with is how you get a system that is confidently
wrong at scale.

### 2. Integration tests against a real Postgres

The repositories are exercised against a mock driver, which proves the SQL is
shaped right and not that it is right. `testcontainers-go` in CI, running the
migrations and the real queries, is the gap between those two claims.

### 3. Notification on trigger and on pending approval

Right now, a pending approval waits in a queue you have to look at. If nobody
looks, it expires after 24h — which is correct behaviour and a bad experience.

The engine should be able to tell you. Deliberately *not* a new channel stack in
the goal engine: a webhook, and core already owns the channels. Something to the
effect of "one goal fell behind, one spend needs you, here is the link".

### 4. A dashboard

The API is complete and unpleasant to read with `curl`. What a dashboard needs to
show, in order: the open approval queue, every goal's pace as a single number, the
last tick's decisions, and the audit log with a filter. Read-mostly; one button
that resolves an approval and one that engages the kill switch.

Not shipped in this repo unless somebody wants to own it — a read-only HTML view of
existing endpoints is a weekend, and keeping a frontend alive is not.

### 5. Per-bot token and compute budgets

The [second risk row](motivation.md#risks), and currently only half addressed.
Trigger budgets bound how often an agent is *woken*; nothing bounds what it spends
once awake. That ceiling lives in core, where the model calls happen.

The shape that fits this project: core reports spend to the goal engine as a metric
(`ops.tokens_spent`, per bot), the existing approval ladder governs the cap, and
exceeding it engages a per-bot pause. Reuses three mechanisms that already exist
rather than inventing a fourth.

### 6. A stronger sandbox

Phase 1 accepts Docker isolation, which is a boundary and not a wall. Agent-authored
code runs in core, so this is core's decision, but it is the largest remaining risk
in the whole system and it belongs on this list.

Options, in the order they were originally weighed: E2B, Daytona, or a dedicated VPS
whose credentials reach nothing that matters. Until one of them, the rule from
[architecture.md](architecture.md): do not give core credentials you would not give
a contractor on their first day.

### 7. Phase 3 — more products, more machines

Applying the goal layer to a second and third product is mostly configuration, and
the interesting question is not technical. It is whether ten goals across three
products produce ten useful triggers a month or a hundred interruptions. Answer
that with two products before assuming it scales to five.

Multi-VPS and a separate sandbox host follow load, not ambition.

## Deliberately absent

These are decisions, not gaps. Reopening one should require an argument.

| | Why not |
|---|---|
| **Phone and SMS** | A [non-goal](motivation.md#non-goals) from the start, and it stays out until the autonomy layer has been boring for several months. A channel that rings is a channel that gets used at 2am for something that could have waited. |
| **Native Windows desktop control** | Phase 3 says "evaluate if a business tool requires it". Nothing has. Remote-controlling a desktop is a large attack surface bought for one tool's sake. |
| **A metric write API** | Definitions carry SQL and the names of variables holding database passwords. An endpoint that created one would be an endpoint that reads any database this service can reach. |
| **An audit mutation path** | Not "admin-only" — no method at all. A log with a delete path is a log somebody deletes. |
| **Agent-facing configuration** | Every limit an agent operates under lives in a file the agent cannot read or write, changed by a restart. A limit that can change without a deploy is a limit that changes quietly. |
| **A task queue in the goal engine** | The monitor is a timer over a bounded batch. A queue adds a moving part to the one layer whose job is to be predictable. |
| **Auto-raising caps** | The system will never widen its own budget in response to hitting it. Ever. |
| **Multi-tenancy** | Self-hosted, single-operator. `product` is a label on a goal, not a tenant boundary, and pretending otherwise would make the audit trail a lie. |
| **A hosted version** | Somebody else's box holding your audit log and your database credentials defeats the point. |

## Contributing to any of this

The pilot (item 1) is the one that unblocks everything else, and it needs an
operator with a real metric, not a contributor with a patch.

For code: integration tests (item 2) are the highest-value thing a newcomer can
pick up, and the hardest to get wrong. See [CONTRIBUTING.md](../CONTRIBUTING.md).

If you want to propose something from the absent list, open an issue with the
failure it prevents. "A missing limit means ask a human" is the kind of decision
that took an argument to arrive at, and it should take one to reverse.
