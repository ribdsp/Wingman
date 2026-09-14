# The goal engine

The part of Wingman that decides when to bother you, and what an agent may do
without asking. This document is the model; [api.md](api.md) is the interface.

- [A goal](#a-goal)
- [Pace, not progress](#pace-not-progress)
- [The evaluation ladder](#the-evaluation-ladder)
- [Rate limits on autonomy](#rate-limits-on-autonomy)
- [Metrics](#metrics)
- [The spending gate](#the-spending-gate)
- [The kill switch](#the-kill-switch)
- [The audit log](#the-audit-log)
- [Telling a human](#telling-a-human)
- [Worked example](#worked-example)

## A goal

```
product      acme
title        MRR to 50M by end of Q4
sourceText   get MRR to 50M before the quarter ends
metricKey    business.mrr          ← must be declared in config/metrics.yaml
comparator   gte                   ← reach it (gte) or stay under it (lte)
targetValue  50000000
baseline     31400000              ← captured when the goal is created
period       2026-10-01 → 2026-12-31
tolerance    0.05                  ← how far off pace is still "fine"
cooldown     6h                    ← minimum gap between triggers
budget       5                     ← triggers allowed in this period
botId        growth                ← who gets woken
```

`sourceText` is kept verbatim because it is what the agent is shown. A brief that
says *"get MRR to 50M before the quarter ends — currently 12% behind pace"* is a
better instruction than one reconstructed from four numeric fields.

`baselineValue` matters more than it looks. Without it, "get MRR to 50M" is
measured from zero and the goal reads as 37% done on day one. With it, progress is
measured across the distance you actually have to travel.

## Pace, not progress

A goal is not judged against its target until its period ends. It is judged against
where it should be *by now*.

```
elapsed  = (now − periodStart) / (periodEnd − periodStart)      clamped to [0,1]
expected = baseline + (target − baseline) × elapsed
band     = |expected| × toleranceRatio

on track (gte):  observed ≥ expected − band
on track (lte):  observed ≤ expected + band
```

Two derived numbers are stored with every evaluation because they are what a human
actually reads:

```
progress = (observed − baseline) / (target − baseline)     1.0 = target reached
pace     = progress / elapsed                             < 1.0 = behind
```

`pace 0.88` says "you have done 88% of the work you should have done by now". It is
the number a dashboard should show.

Edge cases are decided rather than left to the arithmetic:

- **Zero-length period** reads as fully elapsed, so it settles instead of dividing
  by zero.
- **At the instant a period opens**, nothing can be late: pace is 1.
- **A baseline that already satisfies the target** has no ramp to walk, so the
  expectation is the target line itself. A "keep churn under 2%" goal that starts at
  1% is held to 2%, not held to 1% forever.
- **The tolerance band is taken from the magnitude** of the expectation, so a metric
  that legitimately goes negative still gets a sane band.

## The evaluation ladder

Checked in this order, every tick, for every active goal. The order *is* the safety
model, so it is worth reading as one:

| # | Condition | Decision |
|---|---|---|
| 1 | kill switch engaged | `halted` |
| 2 | goal is not `active` | `skipped_inactive` |
| 3 | observed value is NaN or infinite | `skipped_invalid_sample` |
| 4 | period has not started | `skipped_not_started` |
| 5 | target met — and `gte`, or the period has closed | `achieved` |
| 6 | period has closed, target not met | `missed` |
| 7 | within tolerance of expected | `noop` |
| 8 | behind, but the trigger budget is spent | `trigger_budget_exhausted` |
| 9 | behind, but inside the cooldown | `cooldown_skipped` |
| 10 | behind | **`trigger`** |

`trigger` is the only outcome that wakes an agent, and it is the last thing the
ladder can produce. Everything ambiguous stops short of it.

Row 5 is asymmetric on purpose. A "reach 50M" goal is done the moment it is reached.
A "stay under 2%" goal can still regress, so it only settles when its period closes.

Rows 3 and 4 exist because "I could not tell" and "it is fine" must never produce
the same record. A goal whose metric is broken shows up as
`skipped_invalid_sample`, not as `noop`.

Every branch — including the ones that did nothing — writes a row to
`goal_evaluations` with its reason string. A monitor that only records its
interventions cannot answer "was it watching?".

## Reading the verdicts back

Those rows are readable, and reading them is the only supported way to know where a
goal stands:

| | |
|---|---|
| `GET /v1/goals/:id/evaluations` | one goal's history, newest first |
| `GET /v1/evaluations/latest` | the newest evaluation of every goal that has one |

Neither route can write. There is no insert method on the service at all — same rule
as the audit log, for a sharper reason: a row here is a verdict, and a verdict that no
recorded observation supports is worse than no verdict.

**Do not recompute pace.** `paceRatio` as this service computed it is what decided
whether an agent was woken. A dashboard dividing progress by elapsed time for itself
would eventually disagree — different rounding, a different idea of "now", a baseline
captured at a moment it cannot see — and the disagreement would surface as two
plausible numbers with no error between them, which is the failure mode an operator
cannot debug.

A goal the monitor has not reached yet is **absent** from `/v1/evaluations/latest`
rather than present with zeroed numbers. Zero pace reads as catastrophically behind, a
pace of one as on track, and neither is true of a goal that has never been evaluated.
An unknown goal id on the history route is a `404`, not an empty page, for the mirror
of that reason: an empty page means "never once successfully evaluated", which is a
real state worth alarming about.

Reading the last verdict never runs a new one. `POST /v1/monitor/tick` is the route
that evaluates, and it is separate on purpose.

## Rate limits on autonomy

Each trigger costs money (an agent runs, tokens are spent, maybe an API is called)
and attention. Two independent limits bound that, and neither can be turned off:

| | Default | Floor | Meaning |
|---|---|---|---|
| `triggerCooldownSeconds` | 21600 (6h) | 900 (15m) | Minimum gap between triggers for this goal |
| `maxTriggersPerPeriod` | 5 | 1 | Total triggers this goal gets for the whole period |

Omit them and you get the defaults, not zero — zero means "no limit" to the
evaluator, so a goal created without limits would otherwise be the most aggressive
goal in the registry, which is the opposite of what leaving a field blank usually
means.

`MONITOR_MAX_GOALS_PER_TICK` bounds the other direction: how much one tick may do,
ordered by nearest deadline first, so a registry of 10,000 goals degrades into
"the urgent ones are checked" rather than into a tick that never finishes.

## Metrics

A goal can only be measured on a metric declared in `config/metrics.yaml`. There is
no API that creates one — a definition carries SQL text and the name of the
environment variable holding a database password, so an agent that could add one
could read anything this service can reach.

Three sources:

| Source | Who reads it | How |
|---|---|---|
| `push` | you, or a feed you run | `POST /v1/metrics/<key>/samples` |
| `sql` | the engine | one statement, in a read-only transaction, timeout-bounded, against a read-only DSN |
| `http` | the engine | one GET, `jsonPath` into the response body |

The `sql` source is guarded twice: the query text is checked to be a single read,
and the transaction is opened `ReadOnly` — the string check is a first pass, the
transaction is the guarantee. Use a read-only database role anyway.

**Staleness stops evaluation.** If the newest sample for a metric is older than
`METRIC_MAX_SAMPLE_AGE` (26h by default, which allows a daily job to run late), goals
on that metric get `skipped_invalid_sample` instead of being judged. A dead feed
leaves its last number in place, and a goal judged on that number looks exactly as
on-track as it did the moment the feed died.

Push values carry the trust of the credential that sent them. Give every feed its
own bot key. `observedAt` may not be more than 5 minutes in the future or more than
7 days in the past, so a clock-skewed or replayed batch cannot rewrite history.

## The spending gate

Every action that costs money asks first. The ladder is deny-biased:

| Condition | Outcome |
|---|---|
| kill switch engaged | `denied` |
| amount is not a positive finite number | `denied` |
| today's recorded spend is unreadable or negative | `denied` — no deciding against a broken ledger |
| **no policy for this `actionType`** | `pending` |
| policy is `enabled: false` | `denied` |
| currency differs from the policy's | `denied` — no guessing a rate |
| above `hardCap` | `denied` |
| would push today past `dailyCap` | `denied` |
| below `autoApproveBelow` | `auto_approved` |
| anything left | `pending` |

Three of those rows are the whole design:

**A missing policy asks a human.** `config/policies.yaml` ships empty, so on a fresh
install nothing spends money without you. A limit you have not written down is not a
limit of zero and not a limit of infinity — it is a question.

**Caps refuse instead of escalating.** An operator asked to rubber-stamp a cap breach
will eventually rubber-stamp it, and then the cap is decoration. Raising a cap is a
config change you make deliberately, in the morning, not an approval you tap at
midnight.

**Pending expires.** `APPROVAL_TTL` (24h) closes unanswered requests, swept every
5 minutes and once at startup — including requests that expired while the process was
down. Silence is not consent, and an unanswered request that sits forever eventually
gets approved by someone who has forgotten the context.

`idempotencyKey` makes retries safe: the same key returns the original decision
instead of opening a second request. Resolving an already-resolved request is a
`409` — whoever answered first is who decided.

## The kill switch

`PUT /v1/flags/kill-switch` halts every trigger and denies every spend.

**Anyone may engage it.** An agent that works out it is doing damage should be able
to stop the fleet; the worst case is an outage you undo in one call. **Only an
operator may release it** — brakes an agent can disengage are not brakes, and the
situation the switch exists for is exactly the one where the agent's judgement is
what failed. A reason is required in both directions; it is the line somebody reads
while working out why the engine went quiet.

If the flag row cannot be read at all, the engine treats the switch as **engaged**.
Unreadable means halt.

An engaged switch still reports `/readyz` as ready. It was engaged on purpose, and
reporting unready would pull the service out of the load balancer along with the
endpoint that releases it.

## The audit log

Append-only. Not "admin-only" — there is no service method that updates or deletes a
row.

Every entry carries the actor (`system`, `user`, or `bot`, plus the credential's
name), the action, the subject, the outcome, a detail object, and the request ID that
produced it. That last field is what turns one line in a log into one row in the
audit trail.

```
2026-09-11T18:04:11Z  user ops  goal.updated  goal 0f8c…  ok
    {"targetValue": {"from": 50000000, "to": 60000000}}  req 0d9f…
```

Actions: `goal.created`, `goal.updated`, `goal.baseline_captured`,
`goal.trigger_dispatched`, `goal.trigger_failed`, `goal.settled`,
`flag.kill_switch_set`, `approval.decided`, `approval.resolved`, `spend.recorded`,
`metric.sample_recorded`.

An audit write that fails is logged loudly but does not roll back the action it was
recording — a spend that has already been approved is not un-approved by a failed
insert. That trade-off is deliberate and it is the one place where the log can be
incomplete; the service log is the backstop.

## Telling a human

Off by default (`NOTIFY_ENABLED=false`), like `TRIGGER_DRY_RUN` and for the same reason:
one line turns it on, and the honest default is the quiet one.

Two moments produce a notification, and both fire *after* the decision they describe is
already durable:

| When | Headline |
|---|---|
| A trigger was dispatched — the row is marked sent and the audit entry is written | `A goal fell behind pace and an agent has been woken.` |
| A spend request came back `pending`, and only `pending` | `A spend is waiting for a decision (<actionType>).` |

`auto_approved` and `denied` send nothing. An approval that resolved itself is not news,
and a refusal is already recorded where refusals are read.

**A notification carries no figure.** No amount, no currency, no observed value, no pace
ratio — a test asserts a headline contains no digit at all. Two reasons, both worth the
constraint. A chat message is retained on somebody else's servers for as long as they
like, while this project's whole posture is that the business numbers stay on the box. And
a message complete enough to decide from invites deciding from it: approving a spend at a
glance, without the policy, the day's total or the payload in front of you, is the failure
this system is most exposed to. The link is what makes you open the console, where all of
that is. `WEB_BASE_URL` is what builds it; unset, the message goes out without a link
rather than with a broken one.

**It can never affect a decision.** The engine does not own a channel stack — it posts one
line to core, which owns Telegram, Slack and Discord already, and core decides who to tell
and on which platform. That call is best effort: nothing is retried, nothing is queued,
there is no new table in either database, and a failure is logged and dropped. The same
trade-off `appendAudit` makes, for the same reason — by the time it runs, the thing it
describes has happened, so returning an error would only make a caller retry something
that must not be repeated.

A lost message already has a better safety net than a retry: the approval is still in the
queue, the console still shows it, and its TTL still expires it. What is lost is a prompt,
never a decision.

There is also **no new audit action and no tenth error code.** The audit log is a log of
decisions and a ping is not one; "was I told?" is answered by a structured log line
carrying the kind, the subject id and whether a retry could have helped.

## Worked example

A goal to reach 50,000,000 from a baseline of 31,400,000 between 1 October and
31 December, tolerance 5%, cooldown 6h, budget 5.

| Date | Observed | Elapsed | Expected | Pace | Decision |
|---|---|---|---|---|---|
| 1 Oct | 31,400,000 | 0% | 31,400,000 | 1.00 | `noop` |
| 1 Nov | 36,000,000 | 34% | 37,700,000 | 0.73 | `trigger` — 4.5% below the band |
| 1 Nov, +2h | 36,000,000 | 34% | 37,700,000 | 0.73 | `cooldown_skipped` |
| 15 Nov | 41,000,000 | 50% | 40,700,000 | 1.03 | `noop` |
| 20 Dec | 49,000,000 | 88% | 47,800,000 | 1.06 | `noop` |
| 28 Dec | 50,100,000 | 96% | 49,300,000 | 1.04 | `achieved`, goal closed |

The 1 November row is the one the whole service exists for. Nobody asked. Nobody
was looking. The number was 4.5% below where it needed to be, and an agent got a
brief about it before anyone noticed.
