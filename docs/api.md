# Goal engine API

Every route below is served by `goal-engine`. Two of them need a human; the rest
an agent can call for itself. Nothing here creates a metric definition or a
spending policy — those live in the operator's YAML and have no write path.

- [Envelope](#envelope)
- [Authentication](#authentication)
- [Errors](#errors)
- [Health](#health)
- [Reference](#reference)
- [Goals](#goals)
- [Metrics and samples](#metrics-and-samples)
- [Approvals](#approvals)
- [Flags](#flags)
- [Audit](#audit)
- [Monitor](#monitor)

## Envelope

Every response — success or failure — has the same shape.

```json
{
  "success": true,
  "code": 200,
  "message": "Goal created.",
  "data": {},
  "meta": {
    "requestId": "0d9f7c1e-6c2a-4a1b-9c3f-6e5a2b1d4c88",
    "timestamp": "2026-09-11T19:00:00+07:00"
  }
}
```

List responses add pagination to `meta`:

```json
"meta": {
  "requestId": "…",
  "timestamp": "…",
  "pagination": { "page": 1, "limit": 50, "totalItems": 137, "totalPages": 3 }
}
```

Failures replace `data` with `error`:

```json
{
  "success": false,
  "code": 400,
  "message": "The request is not valid.",
  "error": {
    "code": "VALIDATION_ERROR",
    "message": "The request is not valid.",
    "fields": { "targetValue": "must be greater than zero" }
  },
  "meta": { "requestId": "…", "timestamp": "…" }
}
```

`meta.timestamp` is rendered in `TIMEZONE`. Every timestamp you *send* is parsed
as RFC 3339 and must carry an offset.

`meta.requestId` is echoed from `X-Request-Id` when you send one and generated
when you do not. It appears on the response, in the service's logs, and in the
audit row for whatever the request changed — it is how one line in a log becomes
one entry in the audit trail.

Lists take `page` and `limit`. `limit` is clamped by the service; asking for ten
thousand rows gets you a page.

## Authentication

Send the key either way:

```
Authorization: Bearer <secret>
X-API-Key: <secret>
```

A key is an **operator** key or a **bot** key, decided by which environment
variable it was listed in — `GOAL_ENGINE_API_KEYS` or `GOAL_ENGINE_BOT_KEYS` —
and never by anything in the request. There is no way to ask for a higher role.

Two routes require an operator:

```
PATCH /v1/goals/:id
POST  /v1/approvals/:id/resolve
```

`GET /v1/reference` returns that list as `operatorOnlyRoutes`, so a client can
tell which calls will need a human before it tries one.

Releasing the kill switch also requires an operator, but the route is not gated:
`PUT /v1/flags/kill-switch` has to stay open in the engaging direction, because an
agent that notices it is doing damage must be able to stop itself. Only the
release direction is refused, and it is refused inside the service.

`/healthz` and `/readyz` need no credential. A probe that fails during a
credential rotation is a probe that pages you for nothing.

## Errors

Nine codes, and they are stable — a client may switch on them.

| Code | HTTP | Means |
|---|---|---|
| `VALIDATION_ERROR` | 400 | The body or a query parameter is wrong. `error.fields` says which. |
| `UNAUTHORIZED` | 401 | No key, or an unknown one. |
| `FORBIDDEN` | 403 | A valid bot key on an operator-only operation. |
| `NOT_FOUND` | 404 | No such record, route, or method. |
| `UNKNOWN_METRIC` | 400 / 404 | The metric is not declared in `config/metrics.yaml`. |
| `ALREADY_RESOLVED` | 409 | Someone already answered that approval. |
| `RATE_LIMITED` | 429 | Too many requests from this IP or this principal. |
| `INTERNAL_ERROR` | 500 | A bug or a failed dependency. Never carries detail. |
| `SERVICE_UNAVAILABLE` | 503 | A dependency is unreachable; `/readyz` says the same. |

A 500 body never contains a driver message, a DSN, a query, or a host name. When
something breaks, `meta.requestId` plus the service log is the way in.

## Health

### `GET /healthz`

The process is running. Nothing more is claimed.

### `GET /readyz`

```json
{ "status": "ready", "killSwitchEngaged": false, "metrics": 3 }
```

Reads the kill switch, which is the cheapest call that proves the database is
reachable and the flag table readable. **An engaged kill switch is still ready** —
it was engaged on purpose, and reporting unready would pull the service out of the
load balancer along with the endpoint that releases the switch. 503 means a
dependency is down.

## Reference

### `GET /v1/reference`

Every enum this API will send you, so a dashboard does not have to discover them
from traffic: `goalStatuses`, `comparators`, `decisions`, `approvalOutcomes`,
`approvalResolutions`, `operatorOnlyRoutes`.

## Goals

A goal is a target on one metric over one period, plus the limits on how often
falling behind it may wake an agent.

### `POST /v1/goals`

```json
{
  "product": "acme",
  "title": "MRR to 50M by end of Q4",
  "sourceText": "kejar MRR 50 juta sebelum akhir kuartal",
  "metricKey": "business.mrr",
  "comparator": "gte",
  "targetValue": 50000000,
  "periodStart": "2026-10-01T00:00:00+07:00",
  "periodEnd": "2026-12-31T23:59:59+07:00",
  "toleranceRatio": 0.05,
  "triggerCooldownSeconds": 21600,
  "maxTriggersPerPeriod": 5,
  "botId": "growth",
  "channelId": "C0123"
}
```

`metricKey` must name a metric declared in `config/metrics.yaml`; anything else is
`UNKNOWN_METRIC`. `sourceText` is what you actually said, kept verbatim — the
brief an agent receives quotes it rather than the parsed target.

Omitted safety limits default to something restrictive, not to zero, because zero
means "no limit" to the evaluator:

| Field | Default | Bound |
|---|---|---|
| `comparator` | `gte` | `gte`, `lte` |
| `periodStart` | now | — |
| `toleranceRatio` | `0.05` | ≤ `0.5` — a goal tolerant of being half behind is not being watched |
| `triggerCooldownSeconds` | `21600` (6h) | ≥ `900` (15m) |
| `maxTriggersPerPeriod` | `5` | ≥ 1 |
| `status` | `active` | see below |
| period length | — | 1 hour to 5 years |
| `title` | — | ≤ 200 chars |
| `sourceText` | — | ≤ 4000 chars |

`201`, and the goal is echoed back with its `baselineValue` if the metric already
had a reading.

Statuses: `active`, `paused`, `achieved`, `missed`, `archived`. Only `active`
goals are evaluated.

### `GET /v1/goals`

Filters: `product`, `status`, `metricKey`, `search`, plus `page` and `limit`.

### `GET /v1/goals/:id`

### `PATCH /v1/goals/:id` — operator only

Every field optional; only what you send changes.

```json
{ "targetValue": 60000000, "status": "paused", "periodEnd": "2027-01-31T23:59:59+07:00" }
```

Moving the target is an operator's call because the alternative is an agent that
resolves being behind by lowering the bar. Both the route and the service enforce
it; the change is audited as `goal.updated` with the old and new values.

## Metrics and samples

### `GET /v1/metrics`, `GET /v1/metrics/:key`

```json
{ "key": "business.mrr", "description": "Monthly recurring revenue", "unit": "IDR", "source": "push" }
```

That is the whole view, on purpose. It says nothing about *how* a metric is
collected — no query, no URL, no datasource, no credential name. Definitions are
read-only here: they carry SQL, and an endpoint that created one would be an
endpoint that reads any database this service can reach.

### `POST /v1/metrics/:key/samples`

Report a value for a `push` metric.

```json
{ "value": 47250000, "observedAt": "2026-09-11T18:00:00+07:00", "note": "billing nightly job" }
```

`value` is required and `0` is a real reading — an absent `value` is rejected
rather than read as zero. `observedAt` defaults to now, may not be more than
5 minutes in the future, and may not be backdated more than 7 days.

A value for a `sql` or `http` metric is refused with `400`: the engine reads those
for itself, and accepting a pushed number would let a caller overwrite what the
database says. An undeclared key is `404 UNKNOWN_METRIC`.

`201`, and the stored sample is returned. The audit row records which credential
sent it.

> Push metrics are self-reported. Give every external feed its own bot key, so a
> wrong number has one owner. A value older than `METRIC_MAX_SAMPLE_AGE` (26h by
> default) stops goals on that metric from being evaluated at all, rather than
> letting a dead feed's last number read as on-track forever.

### `GET /v1/metrics/:key/samples/latest`

The most recent reading. `404` if nobody has reported one — which is a different
answer from `500`, and the difference is kept.

## Approvals

The gate between an agent deciding to spend money and money moving.

### `POST /v1/approvals`

```json
{
  "actionType": "ads.spend",
  "amount": 250000,
  "currency": "IDR",
  "goalId": "0f8c…",
  "idempotencyKey": "campaign-42-topup-2026-09-11",
  "payload": { "campaignId": "42", "platform": "meta" }
}
```

`201` with one of three outcomes:

```json
{ "outcome": "auto_approved", "policyReason": "below the 100000 IDR threshold", "isOpen": false }
{ "outcome": "pending",       "policyReason": "above the daily cap",             "isOpen": true, "expiresAt": "…" }
{ "outcome": "denied",        "policyReason": "…",                               "isOpen": false }
```

The decision comes from `config/policies.yaml`, and the ladder is deny-biased:

- kill switch engaged → `denied` (still `201`; the request was answered, and the
  answer was no)
- **no policy for that `actionType`** → `pending`. A missing limit means "ask a
  human", never "no limit"
- the type is `enabled: false` → `denied`
- `currency` differs from the policy's → `denied`, rather than guessing a rate
- above `hardCap`, or `dailyCap` minus what has already been spent today →
  `denied` outright, not queued. An operator asked to rubber-stamp a cap breach
  will eventually rubber-stamp it; raising a cap is a configuration change, not an
  approval
- below `autoApproveBelow` → `auto_approved`, recorded and done
- anything left — at or above the threshold but inside the caps → `pending`

Send `idempotencyKey` from anything that retries. The same key returns the same
decision instead of asking twice.

`requestedBy` is accepted but only honoured for an operator filing on an agent's
behalf. A bot is always recorded under the credential it authenticated with — that
field is exactly what a compromised agent would use to spread its spend across
several identities. `payload` is capped at 16 KB: it exists so a human can see
what they are approving, not to park a document.

### `GET /v1/approvals`

Filters: `actionType`, `outcome`, `openOnly=true`, `page`, `limit`.
`openOnly=true` is the queue a human has to work through.

### `GET /v1/approvals/:id`

### `POST /v1/approvals/:id/resolve` — operator only

```json
{ "resolution": "approved", "note": "checked the campaign, go ahead" }
```

`resolution` is `approved` or `rejected`. Answering twice is
`409 ALREADY_RESOLVED` — whoever answered first is who decided, and the second
caller is told rather than silently overriding.

### `POST /v1/approvals/expire`

Closes pending requests past their TTL and returns how many. The service does
this itself every 5 minutes and once at startup; the route exists for a runbook
and for tests.

## Flags

### `GET /v1/flags`, `GET /v1/flags/kill-switch`

```json
{ "key": "kill_switch", "enabled": false, "reason": "", "updatedBy": "ops", "updatedAt": "…" }
```

### `PUT /v1/flags/kill-switch`

```json
{ "engaged": true, "reason": "the growth agent is looping on the same campaign" }
```

Engaging halts every trigger and denies every spend. **Any** credential may
engage it — an agent that notices it is doing damage must be able to stop itself,
and a switch only a human can reach is a switch that waits for morning.

A `reason` is required in both directions: it is the line somebody reads while
working out why the engine went quiet. Releasing the switch additionally requires
an operator key — an agent that could turn its own brakes off would make the
switch meaningless exactly when it matters. If the flag cannot be read at all, the
engine treats the switch as engaged: unreadable means halt.

## Audit

### `GET /v1/audit`

Append-only. There is no write route, and the service has no method that updates
or deletes a row.

Filters: `actorType`, `action`, `subjectType`, `subjectId`, `page`, `limit`.

```json
{
  "id": 4127,
  "at": "2026-09-11T18:04:11+07:00",
  "actorType": "user",
  "actorId": "ops",
  "action": "goal.updated",
  "subjectType": "goal",
  "subjectId": "0f8c…",
  "outcome": "ok",
  "detail": { "targetValue": { "from": 50000000, "to": 60000000 } },
  "requestId": "0d9f…"
}
```

Actions: `goal.created`, `goal.updated`, `goal.baseline_captured`,
`goal.trigger_dispatched`, `goal.trigger_failed`, `goal.settled`,
`flag.kill_switch_set`, `approval.decided`, `approval.resolved`,
`spend.recorded`, `metric.sample_recorded`. Subjects: `goal`, `approval`, `flag`,
`metric`.

## Monitor

### `POST /v1/monitor/tick`

Runs one evaluation pass now instead of waiting for `MONITOR_INTERVAL`.

```json
{
  "startedAt": "2026-09-11T18:00:00+07:00",
  "durationMs": 412,
  "halted": false,
  "checked": 12,
  "triggered": 1,
  "settled": 2,
  "failed": 0,
  "decisions": { "noop": 9, "trigger": 1, "achieved": 2 },
  "errors": []
}
```

`halted: true` means the kill switch is engaged and nothing was dispatched.
`failed` counts goals whose metric could not be sampled — those are reported, not
silently treated as on track.

One decision per goal per tick:

| Decision | Meaning |
|---|---|
| `noop` | On pace. |
| `trigger` | Behind pace; a task was created in core. |
| `cooldown_skipped` | Behind, but triggered too recently. |
| `trigger_budget_exhausted` | Behind, but out of triggers for this period. |
| `achieved` | Target met; the goal is closed. |
| `missed` | Period ended short; the goal is closed. |
| `skipped_not_started` | The period has not begun. |
| `skipped_inactive` | Not `active`. |
| `skipped_invalid_sample` | No reading, or one too stale to judge. |
| `halted` | The kill switch is engaged. |
