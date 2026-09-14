# API

Two services on two ports, sharing one response envelope and one set of error
codes. `goal-engine` decides *whether* an agent should act and *whether* money may
move; `core` decides *how far* one run may go and does the work.

Nothing in either API creates a metric definition, a spending policy or a tool
grant. All three live in operator-owned YAML and have no write path, which is why
neither service's route list contains one.

Both APIs have three kinds of caller, and one of them is the console in `web/`. It
is a client of these routes and nothing more — it has no API of its own, no database
and no state. Its `/api/engine/*` and `/api/core/*` paths forward a fixed list of the
routes below and are not a third API; see [web.md](web.md#the-proxy). Where a route's
behaviour matters to it, this document says so.

**Shared**

- [Envelope](#envelope)
- [Errors](#errors)

**Goal engine** — `:8080`

- [Authentication](#authentication)
- [Health](#health)
- [Reference](#reference)
- [Goals](#goals)
- [Metrics and samples](#metrics-and-samples)
- [Approvals](#approvals)
- [Flags](#flags)
- [Audit](#audit)
- [Monitor](#monitor)

**Core** — `:8081`

- [Who is calling](#who-is-calling)
- [Liveness and reference](#liveness-and-reference)
- [Signing in](#signing-in)
- [Your own account](#your-own-account)
- [Channels](#channels)
- [Accounts](#accounts)
- [Chats and messages](#chats-and-messages)
- [Tasks](#tasks)
- [Runs](#runs)
- [Notifications](#notifications)

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
thousand rows gets you a page. Core's ceiling is 200 rows and its default is 50.

Both services render this envelope from their own copy of the same code, and both
read `TIMEZONE` for `meta.timestamp`. The copy is deliberate: a shared module would
couple two services that deploy independently, for less code than the coupling
costs.

## Errors

Nine codes, and they are stable — a client may switch on them. **Both services use
the same nine**, so a client does not have to learn a second vocabulary halfway
through a conversation with Wingman.

| Code | HTTP | Means |
|---|---|---|
| `VALIDATION_ERROR` | 400 | The body or a query parameter is wrong. `error.fields` says which. |
| `UNAUTHORIZED` | 401 | No key, or an unknown one. In core, also a session token that is not live. |
| `FORBIDDEN` | 403 | A valid credential on an operation its role may not perform. |
| `NOT_FOUND` | 404 | No such record, route, or method. |
| `UNKNOWN_METRIC` | 400 / 404 | The metric is not declared in `config/metrics.yaml`. In core, the same shape of mistake: a model or tool the operator's YAML does not define. |
| `ALREADY_RESOLVED` | 409 | Someone already answered that approval. In core, every 409: cancelling a run that has already stopped, or a second account on one email address. |
| `RATE_LIMITED` | 429 | Too many requests from this IP or this principal. |
| `INTERNAL_ERROR` | 500 | A bug or a failed dependency. Never carries detail. |
| `SERVICE_UNAVAILABLE` | 503 | A dependency is unreachable; `/readyz` says the same. |

Adding a tenth is an API change, not a detail. Core was written after this list
existed and needed none.

A 500 body never contains a driver message, a DSN, a query, or a host name. When
something breaks, `meta.requestId` plus the service log is the way in.

A refusal is not an error. A spend denied under the kill switch is a `201` carrying
`"outcome": "denied"`, and a run halted by the same switch is a recorded run whose
stop reason is `halted`. Both are answers.

## Goal engine

Everything below `:8080`. Two routes need a human; the rest an agent can call for
itself. What it will not do at any privilege level is define a metric or a spending
policy.

### Authentication

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

### Health

#### `GET /healthz`

The process is running. Nothing more is claimed.

#### `GET /readyz`

```json
{ "status": "ready", "killSwitchEngaged": false, "metrics": 3 }
```

Reads the kill switch, which is the cheapest call that proves the database is
reachable and the flag table readable. **An engaged kill switch is still ready** —
it was engaged on purpose, and reporting unready would pull the service out of the
load balancer along with the endpoint that releases the switch. 503 means a
dependency is down.

### Reference

#### `GET /v1/reference`

Every enum this API will send you, so a dashboard does not have to discover them
from traffic: `goalStatuses`, `comparators`, `decisions`, `approvalOutcomes`,
`approvalResolutions`, `operatorOnlyRoutes`.

### Goals

A goal is a target on one metric over one period, plus the limits on how often
falling behind it may wake an agent.

#### `POST /v1/goals`

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

#### `GET /v1/goals`

Filters: `product`, `status`, `metricKey`, `search`, plus `page` and `limit`.

#### `GET /v1/goals/:id`

#### `PATCH /v1/goals/:id` — operator only

Every field optional; only what you send changes.

```json
{ "targetValue": 60000000, "status": "paused", "periodEnd": "2027-01-31T23:59:59+07:00" }
```

Moving the target is an operator's call because the alternative is an agent that
resolves being behind by lowering the bar. Both the route and the service enforce
it; the change is audited as `goal.updated` with the old and new values.

### Evaluations

The recorded output of the monitor's ten-branch ladder — one row per goal per pass,
including the passes that decided to do nothing. Read-only in the strongest sense:
the service has no insert method, for the same reason the audit log has none. A row
here is a verdict, and a verdict no observation supports would be worse than no
verdict at all.

Read them rather than recomputing pace. `paceRatio` is what decides whether an agent
is woken; a client dividing progress by elapsed time for itself would eventually
disagree with the code that made the decision, and the disagreement would be two
plausible numbers with no error between them.

Both roles may read. `page` and `limit` as everywhere else — default 50, maximum 200.

#### `GET /v1/goals/:id/evaluations`

One goal's history, newest first. An unknown goal is `404`, not an empty page: an
empty page means "the monitor has never once managed to evaluate this goal", which is
a real state and an alarming one, and a typo must not look like it.

```json
{
  "id": "9c1e…",
  "goalId": "0f8c…",
  "sampleId": 8841,
  "observedValue": 42000000,
  "targetValue": 100000000,
  "baselineValue": 10000000,
  "expectedValue": 55000000,
  "progressRatio": 0.3556,
  "elapsedRatio": 0.5,
  "paceRatio": 0.7111,
  "onTrack": false,
  "targetMet": false,
  "decision": "trigger",
  "reason": "off pace: observed 4.2e+07 vs expected 5.5e+07 at 50% elapsed (pace 0.71)",
  "evaluatedAt": "2026-09-11T18:00:00+07:00",
  "createdAt": "2026-09-11T18:00:00+07:00"
}
```

`sampleId` is absent when the pass read no sample — a goal skipped as inactive or
not yet started has no observation behind it.

Decisions are the same set `GET /v1/reference` lists: `noop`, `trigger`,
`cooldown_skipped`, `trigger_budget_exhausted`, `achieved`, `missed`,
`skipped_not_started`, `skipped_inactive`, `skipped_invalid_sample`, `halted`.

#### `GET /v1/evaluations/latest`

Where everything stands, in one request: the newest evaluation of every goal that has
one, newest first. This is what a dashboard opens with — a screen that fired a request
per goal is a screen nobody leaves open, and the monitor is only worth running if
somebody notices.

A goal the monitor has not reached yet is **absent**, not present with zeroed
numbers. Zero pace would render as catastrophically behind and a pace of one as on
track; neither is true of a goal that has never been evaluated. `totalItems` counts
goals with history, so it is smaller than the goal count until every goal has been
through a pass.

Reading the last verdict never runs a new one. That is `POST /v1/monitor/tick`, and it
is a separate route on purpose.

### Metrics and samples

#### `GET /v1/metrics`, `GET /v1/metrics/:key`

```json
{ "key": "business.mrr", "description": "Monthly recurring revenue", "unit": "IDR", "source": "push" }
```

That is the whole view, on purpose. It says nothing about *how* a metric is
collected — no query, no URL, no datasource, no credential name. Definitions are
read-only here: they carry SQL, and an endpoint that created one would be an
endpoint that reads any database this service can reach.

#### `POST /v1/metrics/:key/samples`

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

#### `GET /v1/metrics/:key/samples/latest`

The most recent reading. `404` if nobody has reported one — which is a different
answer from `500`, and the difference is kept.

### Approvals

The gate between an agent deciding to spend money and money moving.

#### `POST /v1/approvals`

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

#### `GET /v1/approvals`

Filters: `actionType`, `outcome`, `openOnly=true`, `page`, `limit`.
`openOnly=true` is the queue a human has to work through.

#### `GET /v1/approvals/:id`

#### `POST /v1/approvals/:id/resolve` — operator only

```json
{ "resolution": "approved", "note": "checked the campaign, go ahead" }
```

`resolution` is `approved` or `rejected`. Answering twice is
`409 ALREADY_RESOLVED` — whoever answered first is who decided, and the second
caller is told rather than silently overriding.

The order inside this route is load-bearing and something depends on it: the
operator check runs, then the body is validated, and only then is the approval
looked up. So an id that does not exist answers `404` to an operator key and `403`
to a bot key, without reading or writing anything — no record touched, no audit row.
The console uses exactly that to tell an operator key from a bot key at the moment
one is pasted, instead of finding out at the moment somebody is trying to answer a
spend request ([web.md](web.md#operator-authority)). Reordering the checks so the
lookup came first would turn that probe into a `404` for every key.

#### `POST /v1/approvals/expire`

Closes pending requests past their TTL and returns how many. The service does
this itself every 5 minutes and once at startup; the route exists for a runbook
and for tests.

### Flags

#### `GET /v1/flags`, `GET /v1/flags/kill-switch`

```json
{ "key": "kill_switch", "enabled": false, "reason": "", "updatedBy": "ops", "updatedAt": "…" }
```

#### `PUT /v1/flags/kill-switch`

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

### Audit

#### `GET /v1/audit`

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

### Monitor

#### `POST /v1/monitor/tick`

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

## Core

Everything below is served by `core` on `:8081`. Core accepts work, runs a bounded
loop against a provider using the tools an operator granted, and records every step.

It decides nothing about money: a tool call that spends files a request with the goal
engine's ladder and waits for it. [core.md](core.md) has the loop, the stop ladder and
the tool ladder; this is the wire surface.

### Who is calling

Three kinds of principal, told apart by the shape of the credential and never by
anything in the request:

| Principal | Credential | Is |
|---|---|---|
| operator | a key listed in `CORE_API_KEYS` | whoever runs the instance |
| bot | a key listed in `CORE_BOT_KEYS` | a service — in practice the goal engine's trigger bridge |
| user | a session token from `POST /v1/auth/signin`, prefixed `wgm_` | a signed-in person |

Sent the same two ways as next door:

```
Authorization: Bearer <secret or session token>
X-API-Key: <secret or session token>
```

A token shaped like a session token is resolved against the sessions table; anything
else is compared against the configured keys in constant time. Nothing tries both.
That is what makes the three roles unforgeable in either direction: an environment key
cannot become a user, because it is not shaped like a session token, and a session
token cannot become an operator, because it is never compared against the key list. A
configured key that *is* shaped like one stops the process at startup rather than
looking permanently revoked.

Three routes are operator-only, checked at the route and again in the service:

```
POST /v1/accounts
GET  /v1/accounts
PUT  /v1/accounts/:id/active
```

`GET /v1/reference` returns that list as `operatorOnlyRoutes`, as the goal engine's
does.

Two further rules are enforced in the service rather than at a route, because one
handler serves several kinds of caller:

- **Dispatching unattended work** — `POST /v1/tasks` and `POST /api/v1/tasks` — is for
  an operator and the goal engine. A signed-in person is refused: work nobody is
  watching runs against a budget with nobody at the keyboard, so starting it belongs
  to whoever owns the instance. A person asks for work in a chat instead.
- **Reading a run, its transcript, its cost, or cancelling it** is scoped by who asks.
  A person sees their own; an operator sees every one; the goal engine sees the
  unattended account's and nothing else. A run belonging to somebody else is `404`,
  not `403` — an id that can be confirmed can be enumerated.

`/healthz`, `/readyz`, `POST /v1/auth/register` and `POST /v1/auth/signin` take no
credential. The last two are the only routes where the per-IP limiter is all that
stands between the door and somebody guessing passwords, which is why they sit in a
group with it.

### Liveness and reference

#### `GET /healthz`

The process is running. It touches nothing, so it stays `200` through a database
outage — a liveness probe that fails when a dependency does gets the container
restarted instead of the dependency fixed.

#### `GET /readyz`

```json
{ "status": "ready" }
```

Two things are deliberately **not** part of it: whether the goal engine is reachable,
and whether the kill switch is released. An instance whose engine is down must still
serve the routes a person uses to see what their agent did, and an engaged switch was
engaged on purpose. `503` means core's own dependencies are not reachable, and the
detail is logged rather than returned — a failing pool's error carries the DSN.

#### `GET /v1/reference`

Every enumerated value a client would otherwise learn from traffic: `taskSources`,
`taskStatuses`, `stopReasons`, `stepKinds`, `messageRoles`, `channelKinds`,
`channelsConnected`, `operatorOnlyRoutes`.

`stopReasons` is in **ladder order, not alphabetical**, because that order is the
safety model — a client rendering them in the order given shows a reader the same
precedence the code applies.

`channelKinds` and `channelsConnected` answer different questions and both are needed.
The first is every platform this build can talk to, which is what a `kind` field may
hold. The second is the platforms *this instance* holds a connection to, which is what a
person can usefully be told to connect — a link code minted for a platform nothing is
listening on can never be redeemed. It is `[]` on an instance reached only over HTTP,
never `null`.

Authenticated, but no particular role: it names no account, no task and no run.

### Signing in

#### `POST /v1/auth/register`

```json
{ "email": "you@example.com", "displayName": "Your Name", "password": "…" }
```

`403` unless `CORE_OPEN_REGISTRATION=true`, which is not the default. The check is in
the service, because there is no credential on this route to hang a middleware on. A
password is 12 to 256 characters, hashed with argon2id.

`201` with the account. **No session** — registering and signing in are two steps, so
a client cannot end up holding a token it did not ask for.

On a closed instance the way in is the CLI:

```bash
read -rs PW && printf '%s' "$PW" | core createuser -email you@example.com -name 'Your Name'
```

The password is read from stdin and never appears in a process listing or a shell
history.

#### `POST /v1/auth/signin`

```json
{ "email": "you@example.com", "password": "…" }
```

```json
{
  "token": "wgm_…",
  "expiresAt": "2026-09-19T18:00:00+07:00",
  "user": { "id": "…", "email": "…", "displayName": "…", "isActive": true, "createdAt": "…" }
}
```

`token` is returned **once**: only its hash is stored, so a client that loses it signs
in again and a database dump is not a set of live sessions. Lifetime is `SESSION_TTL`
(168h by default, 15m to 90 days).

The user agent and the address recorded against the session are taken from the request,
not the body. A client that could name its own would be naming what the session list
later shows somebody about their own devices.

Every credential failure is the same `401` with the same sentence. Which of "unknown
address", "wrong password" and "deactivated account" it was is not distinguished, and
sign-in costs the same either way.

#### `POST /v1/auth/signout`

Ends the session the caller presented — no id in the body, because ending your own
session should not require knowing its id, and a route that took one would be a way to
sign somebody else out. A machine key here ends nothing and still answers `200`: a
sign-out that failed would leave a client unable to clear its own state.

### Your own account

User-only, every one of them, and scoped to the caller.

#### `GET /v1/me`

The account. There is no password field of any kind — not the hash, not a placeholder.

#### `PUT /v1/me/password`

```json
{ "currentPassword": "…", "newPassword": "…" }
```

Knowing the current one is what makes this a password change rather than a session
takeover. It ends **every** session, including the one making the request.

#### `GET /v1/me/ledger`

```json
{ "isReadable": true, "tokensToday": 41822 }
```

`isReadable: false` is not "nothing spent" — it is "no answer". A client that showed
`0` for it would tell somebody their budget is untouched when what happened is that the
counter could not be read, and that is the condition under which a run stops rather
than guessing.

The ledger is per account, so this route has no operator form. Instance-wide totals
belong to the goal engine, through the samples core pushes to `ops.tokens_spent`.

#### `GET /v1/me/sessions`

Live sessions and ended ones alike, with the address and user agent each was created
from — recognising an unfamiliar sign-in is the whole purpose of the list. No token and
no hash of one. `isLive` is rendered rather than left to a client to derive from three
timestamps.

No pagination block: the service does not count the table, and an invented total is
worse than none.

#### `DELETE /v1/me/sessions`

Sign out everywhere, including this device. Sparing the current one would defeat the
button.

#### `DELETE /v1/me/sessions/:id`

Somebody else's id is `404`, because the update is scoped on the caller's account
rather than reading the row and comparing.

### Channels

User-only, all three. This is the signed-in half of connecting a chat account: ask for a
code, read back what is connected, disconnect one. A machine key owns no chat account and
is refused at the route.

The other half — attaching an external id to an account — has **no route at all**. It
happens on the inbound path, when a code arrives *over* the channel, because that message
is the proof. For the same reason there is no channel webhook endpoint anywhere in this
document: every connection core makes to Telegram, Slack and Discord is outbound, so
there is nothing for one to receive. [core.md](core.md#channels) has the inbound ladder.

#### `POST /v1/channels/link-codes`

No body. Not even a platform: the code is good on whichever channel it arrives over,
because what proves the link is the message carrying it.

```json
{ "code": "WGM-7B3KD-QMXPZ", "expiresAt": "2026-09-12T18:15:00+07:00" }
```

`201`, and the code exists in that response and nowhere else. Only a SHA-256 hash is
stored and no route reads one back: somebody who loses it mints another, which is cheaper
than an endpoint that would turn a session into a standing supply of channel credentials.

Minting invalidates whatever that account had outstanding, in the same statement — one
live code per person, so a code left in a scrollback is dead as soon as its owner asks for
another. Lifetime is `CHANNEL_LINK_CODE_TTL`: 15 minutes by default, floored at 1 minute
and capped at 1 hour, in the service as well as in the parser.

The grouping is for reading aloud and typing back. The inbound path strips separators and
uppercases, so `wgm 7b3kd qmxpz` sent to the bot is the same code — but the whole message
has to be the code, and nothing else.

#### `GET /v1/channels`

```json
[
  { "id": "…", "kind": "telegram", "externalId": "24680…", "displayName": "Rama",
    "linkedAt": "…", "revokedAt": null, "isLive": true }
]
```

Revoked rows are included: a chat account that was linked and released is the answer to
"why can I not connect this again". `isLive` is rendered rather than left to a client to
derive from `revokedAt` — two clients working out "can this still send me anything?"
independently is two chances to work it out differently.

`externalId` is rendered because it is the only thing that tells two accounts on one
platform apart, and the person reading this list is the person deciding which to
disconnect. It is their own id on their own list, the same reasoning that puts an address
on the session list.

No pagination block, on the session list's precedent.

#### `DELETE /v1/channels/:id`

```json
{ "id": "…", "revoked": true }
```

The row is revoked, not deleted, so the runs that chat account started keep their
provenance. After this the sender is a stranger again: their next message gets the same
answer any unlinked sender gets, and nothing they send starts a run. That is the whole
reason this route exists — it is how somebody takes their phone out of the loop.

Twice is `404`, and so is somebody else's id. The update is scoped on the caller's account
instead of reading the row and comparing, and a `403` would confirm the id exists.

There is no update route and the service has no method for one. A connection is made by
proving it and ended by revoking it; editing one would mean moving somebody else's chat
account onto this account.

### Accounts

Operator only, all three.

#### `POST /v1/accounts`

The same body as registration, honoured whether or not registration is open.

#### `GET /v1/accounts`

A page of accounts.

#### `PUT /v1/accounts/:id/active`

```json
{ "isActive": false }
```

Required, and a pointer in the code so an omitted field is a `400` rather than a silent
deactivation. Deactivating ends every session the account holds — which is the
difference between this and deleting the row: the person is locked out now, and what
their agent did is still in the audit trail.

### Chats and messages

User-only. A chat carries no `userId` in its rendered form: every chat a caller can see
is their own, because the service scopes on the account in the `WHERE` clause.

```
POST   /v1/chats                  { "title": "…" }
GET    /v1/chats                  ?includeArchived=true
GET    /v1/chats/:id
PUT    /v1/chats/:id/title        { "title": "…" }
POST   /v1/chats/:id/archive
GET    /v1/chats/:id/messages     oldest first
```

There is no delete. A chat's messages are the visible half of an append-only run
transcript, so a chat is archived and stays readable.

#### `POST /v1/messages`

```json
{ "chatId": "0f8c…", "text": "look at why signups flattened last week" }
```

`chatId` may be omitted, in which case a chat is created and named after the first line
of the message — the first message of a conversation costs one round trip, not two.
`text` is required and capped at 16 000 characters. An archived chat is `409`.

`201` with all three things that happened:

```json
{
  "chat":    { "id": "…", "title": "look at why signups flattened last week", "…": "…" },
  "message": { "id": "…", "chatId": "…", "role": "user", "content": "…", "…": "…" },
  "task":    { "id": "…", "source": "user", "status": "queued", "…": "…" }
}
```

The task's brief is the last ten messages of the conversation — 800 characters each,
the whole thing under the brief cap — plus what was just said, with the history
labelled as a record and the new message as the request. Text somebody else wrote is
still text, and a line in it that reads like an instruction is not one.

An assistant message carries the `runId` that produced it, so a client can put an
answer next to the transcript behind it. That is how somebody checks what their agent
actually did rather than what it said it did.

### Tasks

#### `POST /api/v1/tasks`, `POST /v1/tasks`

The same handler on both paths. `/api/v1/tasks` is what the goal engine is hardcoded to
call; two dispatch routes would be two places for the idempotency rule to be got wrong,
and that rule is what stops a retried trigger becoming a second run.

```
Authorization: Bearer <bot or operator key>
Idempotency-Key: goal-0f8c-2026-w37
```

```json
{
  "botId": "growth",
  "channelId": "C0123",
  "brief": "MRR is 12% behind pace with 19 days left. Original wording: …",
  "idempotencyKey": "goal-0f8c-2026-w37",
  "metadata": { "goalId": "0f8c…", "metricKey": "business.mrr" }
}
```

The key may arrive in the header, in the body, or both. Both and disagreeing is a `400`
rather than a guess: picking one would leave the caller's retry logic and core's
deduplication keyed on different strings, which is exactly the state in which a retry
becomes a second run. It is required — a caller that omits it is told, rather than
quietly given at-least-once semantics.

- **`201`** and a new task.
- **`200`** and the *original* task, when that key has been seen. The difference is
  what keeps a timed-out trigger that was in fact received from looking like a fresh one
  in either service's logs. The brief is not compared: a key arriving with different
  text still answers with the original task, because the engine derives its keys from a
  goal and a period, so two briefs under one key means the goal was edited between
  attempts.

| Field | Required | Bound |
|---|---|---|
| `brief` | yes | ≤ 16 000 characters. A brief is an instruction, not a document — anything longer belongs in a file the sandbox reads, which costs one tool call instead of every prompt |
| `idempotencyKey` | yes | ≤ 200 characters |
| `metadata` | no | ≤ 32 entries, ≤ 512 characters each. It exists to correlate a task with the goal that caused it, not to carry state between services |
| `botId`, `channelId` | no | which persona ran and where to report |

The task is filed against the account named by `CORE_UNATTENDED_OWNER`, resolved at
boot. A machine key may not name its own owner in the request: one that could would be
filing work against anybody's budget.

```
GET /v1/tasks            the caller's own, newest first
GET /v1/tasks/:id
GET /v1/tasks/:id/runs   usually one; a run a dead worker abandoned is requeued as a second
```

Statuses: `queued`, `running`, `succeeded`, `failed`. One stop reason means the work got
done and ten mean it did not, and the mapping lives in one place so `halted` never gets
filed as a success.

### Runs

#### `GET /v1/runs/:id`

```json
{
  "id": "…",
  "taskId": "…",
  "provider": "anthropic",
  "model": "claude-sonnet-5",
  "stop": "completed",
  "reason": "the model produced a final answer",
  "isInFlight": false,
  "isCancelled": false,
  "limits": {
    "maxIterations": 15,
    "maxToolCalls": 40,
    "maxTokensPerRun": 250000,
    "maxTokensPerUserDay": 2000000,
    "stepTimeoutSeconds": 180,
    "sandboxTimeoutSeconds": 90
  },
  "state": { "iterations": 4, "toolCalls": 2, "tokensUsed": 18422 },
  "startedAt": "…",
  "finishedAt": "…"
}
```

`limits` is the snapshot the run was actually held to, not today's configuration: an
operator who raised a cap yesterday should still see what last week's run was bounded
by. `maxTokensPerUserDay` is `null` when this instance sets no daily cap, rather than
`0` or a sentinel — zero would read as "nothing allowed", which is the opposite.

`provider` and `model` record what answered, not what was asked for; a run that fell
back to a cheaper model produced output that has to be read differently. Both are empty
on a run that stopped before reaching one — halted by the kill switch, say.

The eleven stop reasons and the order they are checked in are in
[core.md](core.md); `GET /v1/reference` returns them in that same order.

#### `GET /v1/runs/:id/steps`

The transcript, oldest first. One step is a model call or a tool call:

```json
{
  "index": 3,
  "kind": "tool",
  "toolName": "shell",
  "content": "…",
  "err": "",
  "tokensIn": 0,
  "tokensOut": 0,
  "at": "…"
}
```

`err` is rendered because a step that failed is part of the record. `content` is capped
at 32 000 characters — a tool that prints a megabyte of log is truncated rather than
turning the transcript into the largest table in the database. Tokens are zero on a tool
step: a sandbox does not bill tokens.

There is no update or delete route, and the service has no method for either.

No pagination block, for the same reason as the session list.

#### `GET /v1/runs/:id/cost`

```json
{
  "charges": [
    { "runId": "…", "provider": "anthropic", "model": "claude-sonnet-5",
      "tokensIn": 12480, "tokensOut": 942, "total": 13422, "occurredAt": "…" }
  ],
  "total": 18422
}
```

Charge by charge as well as the total, because a run's cost is several model calls and
which one was expensive is the useful part.

#### `POST /v1/runs/:id/cancel`

```json
{ "id": "…", "cancelRequested": true }
```

A request, not a kill: the loop reads the flag before each iteration, so the run ends
with its transcript intact and its counters matching what it actually spent. Killing the
process mid-call would leave a charge nobody recorded.

Asking twice is `200` — somebody clicking again because nothing has visibly happened is
not making a mistake. A run that has already finished is `409 ALREADY_RESOLVED`, the one
case where the honest answer is that this is no longer a thing you can do.

### Notifications

#### `POST /v1/notifications`

The second route the goal engine calls, and the only one that is not a dispatch. It is
how "a goal fell behind" or "a spend is waiting" reaches a chat platform the operator
already uses, instead of waiting in a queue somebody has to think to open.

```
Authorization: Bearer <bot or operator key>
```

```json
{
  "kind": "trigger",
  "subjectId": "0f8c…",
  "headline": "A goal fell behind pace and an agent has been woken.",
  "link": "https://wingman.example/goals/0f8c…"
}
```

```json
{ "recipients": 1 }
```

| Field | Required | Bound |
|---|---|---|
| `kind` | yes | `trigger` or `approval_pending`. A closed set: core does nothing with it beyond logging it, but an unknown kind means the two services disagree about their contract, and hearing that from a `400` beats hearing it from a message nobody understands |
| `subjectId` | yes | ≤ 200 characters. The goal or the approval this is about |
| `headline` | yes | ≤ 500 characters. The sentence a person reads, written by the engine. Core adds no wording of its own |
| `link` | no | ≤ 500 characters, and `http://` or `https://` only. A `javascript:` or `file:` link is a phishing message with this instance's name on it, so it is refused at the boundary rather than sent |

**There is no recipient field, and adding one would be an API change with an argument
attached.** Core sends to the live chat identities of the account `CORE_UNATTENDED_OWNER`
names — the same account unattended work is filed against. A field naming who to message
would let whoever holds a bot key send a message from this instance to any chat account
linked to it.

- **`200`** and a count. The count and not the platforms: which platforms somebody has
  connected is their own business, and the engine's only use for the answer is a log line.
- **`200` with `recipients: 0`** when the owner has connected no chat account, or no owner
  is configured. Not an error — that is an ordinary instance, and a retry would find the
  same empty list. The envelope's `message` says so plainly ("Nobody was notified: no chat
  account is connected to the unattended owner."), which is how an operator who turned
  notifications on and heard nothing learns the linking step is still outstanding.
- **`403 FORBIDDEN`** for a signed-in person, operator session included. The same rule as
  dispatch and for the same reason: a person must not be able to make this instance send a
  message to the operator's own phone. Enforced in the service rather than by a route
  guard, because who may do this is one decision and belongs in one place.
- **`500 INTERNAL_ERROR`** when every platform failed. The platform's own message is
  logged and not returned — a rejected token comes back from Telegram with the token in
  it.

A notification is a message, not a record. **There is no `GET /v1/notifications`** and no
table behind it: the engine's audit log already says a trigger fired and an approval is
pending, and a second list of the same events, readable with a bot key and scoped to no
account, would be a second answer to the same question. `GET` there is `405`.

The engine's side of this is `Notify` in `goal-engine/internal/core/client.go`, fired
*after* the decision it describes is already durable. It is best effort: the failure is
logged and dropped, never retried and never queued. See
[goal-engine.md](goal-engine.md#telling-a-human) for when one is emitted, and
[core.md](core.md#who-hears-about-it) for who receives it.


