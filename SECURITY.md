# Security

Wingman lets an autonomous agent act on a business and spend money. The parts of it
worth attacking are obvious, so here is what the project claims, what it does not,
and how to report a hole in either.

- [Reporting a vulnerability](#reporting-a-vulnerability)
- [Scope](#scope)
- [Threat model](#threat-model)
- [What the goal engine guarantees](#what-the-goal-engine-guarantees)
- [What it does not](#what-it-does-not)
- [Hardening a deployment](#hardening-a-deployment)
- [Known limitations](#known-limitations)

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private reporting: **Security → Report a vulnerability** on
[the repository](https://github.com/ribdsp/wingman/security/advisories/new). If that
is unavailable to you, open an issue titled "security contact request" with no
detail in it, and a private channel will be arranged.

Include, as much as you have:

- What you did and what happened.
- Which component — `goal-engine`, the compose stack, the docs, or `core`.
- The commit or tag.
- What an attacker gets out of it.
- A reproduction, if you have one. A failing test in `internal/domain` is ideal.

What to expect: an acknowledgement within a few days, an assessment, and a fix or a
documented "this is intended, here is why". This is a small project without an
on-call rotation — an honest timeline is days for a reply and longer for a fix,
depending on severity.

If you would like credit in the advisory, say so. If you would rather not be named,
say that instead.

Please do not test against anyone else's deployment. Self-host it; the whole thing
comes up with one compose command.

### Reporting against core

`core/` is a fork of [Rakazo](https://github.com/elie222/rakazo). A vulnerability in
upstream's code should go to upstream — they have more users than this project does
and their fix reaches all of them. A vulnerability in *how Wingman integrates with*
core (the task route, the shared secret, the trust placed in `metadata`) is ours;
report it here.

## Scope

**In scope**

- `goal-engine/` — the API, authentication, the two privilege levels, the evaluation
  and approval ladders, the kill switch, the audit log, SQL construction, the metric
  samplers.
- `deploy/docker-compose.yml`, `goal-engine/Dockerfile` — anything that exposes a
  port, leaks a secret into an image layer, or runs something as root.
- `scripts/fork-core.sh`.
- Documentation that tells an operator to do something unsafe. A wrong instruction
  in [deployment.md](docs/deployment.md) is a real vulnerability, and it is the
  cheapest kind to fix.

**Out of scope**

- Upstream Rakazo bugs — report those upstream.
- A deployment you configured insecurely (`0.0.0.0` bindings, a shared operator key,
  a read-write DSN for a `sql` metric). Worth an issue if the docs led you there.
- Missing rate limits on `/healthz`.
- Anything requiring an operator key you were legitimately given. An operator can
  raise every cap by editing a file; that is the design.
- Denial of service by holding a valid credential and hammering the API.

## Threat model

The adversary this system is built against is **not** primarily a remote attacker.
It is a **capable agent behaving badly** — looping, misreading a metric, being
prompt-injected by a webpage it read, or simply wrong. Remote attackers matter too,
and are the more conventional half.

| Adversary | Has | Should not be able to |
|---|---|---|
| **A misbehaving or prompt-injected agent** | A bot key, and the ability to make arbitrary calls with it | Move a goal's target, define a metric, resolve its own approval, release the kill switch, raise a cap, spend past a cap, wake itself faster than the cooldown, alter the audit log |
| **A stolen bot key** | The same | The same, plus: report metric values that are indistinguishable from a legitimate feed's — see [limitations](#known-limitations) |
| **A stolen operator key** | Everything a human operator has | Nothing is claimed. This is game over, and the audit log's only job is to say what was done with it |
| **Unauthenticated network access** | Reachability | Anything at all except `/healthz` and `/readyz` |
| **A compromised core** | The task route's credentials, and whatever core's sandbox reaches | Change any decision in the goal engine. Core is a client of this API, not a peer |
| **A read-only database credential for a metric** | One query against your product database | Write anything, or be used for a second statement |

The first row is the one that shaped the codebase. Almost every design decision in
[goal-engine.md](docs/goal-engine.md) is an answer to "what if the agent is wrong
about this?".

## What the goal engine guarantees

Each of these is enforced in code, not documented as a convention.

**Privilege is fixed at startup.** A credential is an operator key or a bot key
according to which environment variable listed it — `GOAL_ENGINE_API_KEYS` or
`GOAL_ENGINE_BOT_KEYS`. No header, body field, query parameter or audit field can
raise a role. There is no privilege-escalation path because there is no privilege
grant path.

**Operator-only operations are checked twice.** `middleware.RequireOperator` guards
the route; the service checks again through `Actor.requireOperator`. A route guard
protects a route. The service guard protects the invariant when a second route, a
CLI, or a scheduled job reaches the same operation.

**An agent cannot move its own goalposts.** Editing a goal's target, period or
status is operator-only. The alternative is an agent that resolves being behind by
lowering the bar.

**An agent cannot define a metric.** Definitions carry SQL text and the names of
environment variables holding database passwords. They exist only in
operator-owned YAML, read once at startup, with no API write path. The public
metric view publishes the key, description, unit and source — never a query, URL,
datasource or credential name.

**An agent cannot set its own limits.** Spending policies live in the same
operator-owned YAML. A goal may only reference a declared metric.

**A missing policy asks a human.** `config/policies.yaml` ships empty. An action
type with no policy is `pending`, never auto-approved. A limit nobody wrote down is
a question, not permission.

**Caps refuse rather than escalate.** Past `hardCap` or `dailyCap` the request is
denied outright, not queued for approval. An operator asked to rubber-stamp a cap
breach will eventually rubber-stamp it.

**A broken ledger denies.** If today's recorded spend cannot be read, or reads
negative, every request is denied. No deciding against a ledger that is not
answering.

**The kill switch is one-way for agents.** Any credential may engage it — an agent
that notices it is doing damage must be able to stop the fleet. Only an operator may
release it. If the flag row cannot be read at all, the engine treats the switch as
engaged: unreadable means halt.

**The audit log is append-only.** Not "admin-only" — there is no service method that
updates or deletes a row. Every entry carries the actor, the credential's name, the
action, the subject, the outcome and the request ID.

**SQL is parameterised.** No ORM, no string-built queries, one repository type per
table. Metric queries from YAML are checked to be a single read *and* executed in a
`ReadOnly` transaction with a timeout — the string check is a first pass, the
transaction is the guarantee, and a read-only database role is what you should rely
on.

**Errors do not leak.** A 500 body never contains a driver message, a DSN, a query
or a host name. Secrets are never logged: not API keys, not DSNs (only datasource
names), not the core API key.

**Requests are rate-limited** per IP and per principal, and every trigger is bounded
by a per-goal cooldown and a per-period budget whose defaults are restrictive and
whose floors cannot be configured away.

**The container is minimal.** Distroless static base, `nonroot` user, no shell, no
package manager, no `.env` in any layer, published ports bound to loopback.

## What it does not

Stated plainly, because a security document that only lists strengths is marketing.

- **It does not defend against a stolen operator key.** An operator can do
  everything. The audit log records what was done; that is the entire mitigation.
- **It does not sandbox agent code.** That happens in core. Phase 1 accepts Docker
  isolation, which is a boundary rather than a wall. Do not give core credentials
  you would not give a contractor on their first day.
- **It does not validate that a metric is *true*.** A `push` metric is whatever its
  credential says it is.
- **It does not encrypt anything at rest.** The database holds your metrics and
  audit trail in plain tables. Use disk encryption and a backup process you trust.
- **It does not authenticate core's response.** A compromised core can lie about
  having accepted a task. It cannot change a decision, because no decision is made
  there.
- **It does not stop an agent doing damage that costs nothing.** The approval gate
  is about money. An agent that sends a regrettable email is core's problem, and
  core's approval features are the answer.
- **It has no vulnerability-disclosure history**, because it has no production
  history. Treat the guarantees above as claims that have been reasoned about and
  unit-tested, not as claims that have survived contact with an attacker.

## Hardening a deployment

In rough order of how much each one buys you:

1. **Separate keys per caller, named after who they are.** One operator key for you,
   one bot key per agent, one bot key per metric feed. An audit trail that can only
   say "some valid key" does not answer "who moved that target".
2. **Read-only database roles for `sql` metrics.** The engine's read-only
   transaction is a guarantee about the engine, not about the credential.
3. **Do not publish the port.** The compose file binds to `127.0.0.1`. If your
   agents run on the same host, leave it there. Docker writes its own iptables rules
   and will happily bypass a ufw that says otherwise.
4. **Set `TRUSTED_PROXIES` to your proxy and nothing else.** Empty behind a proxy
   means every caller shares one rate-limit bucket. Too broad means a client can
   send its own `X-Forwarded-For` and choose its bucket.
5. **Keep `TRIGGER_DRY_RUN=true` until you have read a week of decisions.** The
   cheapest security control in the project is not letting it act yet.
6. **Add spending policies one action type at a time**, with a small
   `autoApproveBelow`. The empty default is correct; do not fix it before you have
   seen real requests.
7. **`chmod 600` your `.env`**, and back it up encrypted and out of git.
8. **Back up the database.** It is your record of what the agent did.
9. **Rotate keys by adding, restarting, moving the caller, removing, restarting.**
   Both are valid in between, so nothing has an outage.
10. **Pin core to a tag** and read the upstream log before merging. Engage the kill
    switch during the upgrade.

## Known limitations

Design trade-offs, documented rather than hidden. None is a vulnerability report;
all of them are places a reader might reasonably expect something stronger.

**Push metrics are self-reported.** A credential that can report `business.mrr` can
report any number for it. A goal judged on a fabricated value behaves exactly as if
the value were real. Mitigations: one key per feed, so a wrong number has one owner;
`observedAt` may not be more than 5 minutes ahead or 7 days behind, so a
clock-skewed or replayed batch cannot rewrite history; and a value older than
`METRIC_MAX_SAMPLE_AGE` stops evaluation rather than reading as on-track. There is
no cryptographic feed attestation, and adding one would not change the fact that the
feed computes the number.

**An audit write that fails does not roll back the action.** A spend already
approved is not un-approved by a failed insert. The failure is logged loudly. This
is the one place the audit log can be incomplete, and the service log is the
backstop. The alternative — refusing an approved action because a log write failed —
was judged worse.

**Config changes need a restart.** Both YAML files are read once at startup. That is
deliberate: a limit that can change without a deploy is a limit somebody can change
quietly. It also means a compromised host with write access to the config and the
ability to restart the process can change your limits — at which point the host is
the problem.

**`sql` metric queries are operator-supplied SQL.** They are checked to be a single
read and run in a read-only transaction, but an operator who writes a slow query has
written a slow query. `METRIC_SAMPLE_TIMEOUT` bounds it.

**Rate limiting is in-process.** Fine for one instance, which is the documented
deployment. Two instances behind a load balancer each get their own bucket.

**No CSRF protection, and none needed.** The API takes a bearer token or
`X-API-Key`, not a cookie. If you put a browser UI in front of it, that UI's session
handling is your responsibility.

**The race detector runs only in CI.** `make test-race` needs cgo and a C toolchain,
so it is a separate target from `make test`. CI runs it on every push; nothing merges
unraced.
