# Roadmap

What exists, what is next, and what is deliberately absent. Written to be honest
rather than optimistic — this project's whole argument is about restraint, and a
roadmap that promises everything would undercut it.

No dates. This is one person's project plus whoever shows up.

## Where it is now

| | |
|---|---|
| **goal-engine** | Built and unit-tested at 93.8%. The decision core is pure and covered by hand-written scenarios; the HTTP layer, repositories and workers are wired and boot cleanly. Never run in production. |
| **core** | Built and unit-tested at 86.6% whole-module, 92.4% excluding `cmd`. The agent loop, both providers, three tool sources, two sandbox backends, three chat platforms, accounts and budgets. Never run in production. |
| **web** | Built. 229 tests over the pure modules — sealing, the route allow-list, the envelope, pace, formatting; the screens themselves are proved by the type checker and the build rather than by tests, which is a stated limit and not a claim of coverage. Never run in production. |
| **The three together** | Specified on every side, dry-run-ready, and never exercised end-to-end by the author against two live databases and a browser. |

Read that last row twice before you plan around this. `TRIGGER_DRY_RUN=true` is the
default because it is also the only mode anybody has spent real time in.

### Done, in the goal engine

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
- Evaluation history: the recorded verdicts are readable per goal and as one row per
  goal, so nothing downstream has to recompute pace and disagree with the evaluator.
- Notification on trigger and on pending approval: one short message on a chat platform
  the operator already uses, off by default, carrying a console link and no figure.
  Emitted after the decision it describes is durable, never retried, and unable to affect
  one.
- Two privilege levels fixed by which environment variable held the key.
- Packaging: distroless image with a self-probing healthcheck, compose stack,
  migrations on boot.

### Done, in core

- The agent loop: plan, call a tool, observe, repeat — with an eleven-reason stop ladder
  whose order is the safety model and one test per branch.
- Run limits with restrictive defaults and hard floors: iterations, tool calls, tokens
  per run, tokens per person per day, a step timeout, a sandbox timeout. Zero means
  unset; `-1` on the daily cap is the only way to remove one and it must be written out.
- Two providers behind one port, both official SDKs: Anthropic and OpenAI.
- Three tool sources: a shell tool that runs in the sandbox, MCP servers
  (`mcp__<server>__<tool>`), and HTTP endpoints an operator declared. All three read
  operator-owned YAML, all three ship declaring nothing.
- An eight-row tool ladder: no grant denies, a switched-off grant denies, a spending
  tool with no policy contract denies, an unattended write is refused for that run, and
  a run's grants are snapshotted immutably when it starts.
- Two sandbox backends: `docker` (throwaway container, no network, no capabilities,
  read-only root, memory and pid limits, one workspace per person) and `local`, which
  says in its own source that it is not isolation.
- Accounts: closed registration, `core createuser` reading the password from stdin,
  argon2id at OWASP parameters, opaque session tokens stored hashed, constant-cost
  sign-in, every query scoped by `user_id`.
- Three chat platforms behind one port: Telegram, Slack and Discord, every connection
  outbound, so nothing has to be opened to the internet. A seven-rung inbound ladder,
  and a message from a sender nobody has linked reaches exactly one thing — the link
  check.
- Link codes: minted while signed in, single use, minutes long, stored hashed, and shown
  once. An unlinked chat gets a code to redeem, never a session.
- The reverse direction: the kill switch read fail-closed before every run and between
  iterations, spending filed with the engine's ladder, and token use reported back as a
  metric sample.
- Notifications received from the engine and delivered to the unattended owner's linked
  chat accounts — no recipient in the request, revoked identities skipped, one platform's
  failure not costing the others their message, and no history route behind it.
- Sweeps that close work a dead worker left behind, expire sessions and drop spent link
  codes.
- Packaging: alpine image (the sandbox backends look up an external binary at startup, so
  not distroless) running as uid 65532, with a self-probing healthcheck.

### Done, in the console

- The deck: the open approval queue, every goal's pace as one number, the last tick's
  decisions, the most recent audit entries and a chat, on one screen, in that order.
- Eight more screens behind it: goals and one goal, chats and one chat, one run, the
  audit log with a filter, and the screen where operator authority is taken.
- Three credentials, none of which a browser ever sees: a person's core session and a
  pasted operator key, both sealed with AES-256-GCM into httpOnly cookies, and the
  engine bot key, which stays in the server's environment. Every read happens on the
  server.
- No operator key in its environment, deliberately: an operator pastes theirs, it is
  verified before it is held, and it lapses on a timer that does not renew.
- A fixed allow-list of the routes it may forward — 36 of them — instead of a
  pass-through proxy, with the three that may write anything on the goal engine named
  one by one and the authority each needs decided in the module rather than by the
  caller.
- Fail-closed where the services are: an unreadable kill switch reads as engaged and
  the whole console goes to its halted state, while a panel whose read failed says so
  and keeps its last value rather than 500-ing the screen around it.
- A per-request CSP nonce, because a streamed React tree cannot run under a static
  `script-src 'self'` and the failure is silent — the page renders and every button
  is dead.
- No data of its own: no database, no volume, no state, no migrations. Delete it and
  both services are unchanged.

The whole path exists as software: a goal stated, a gap detected, a task filed, a run
claimed, a tool called in a sandbox, a spend requested and approved, everything recorded
— and now a screen to watch it on. What has not happened is anybody watching it happen
against real infrastructure.

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

### 3. Desktop, then mobile

[Phase 3](motivation.md#phases) asked for a web client first, and that is the item
above: `web/` shows the approval queue, every goal's pace, the last tick's decisions,
the audit log with a filter and a chat, resolves an approval, moves a target and works
the kill switch. What it has not had is a month of somebody living with it — which
screen they actually open, and which number they wanted on it and did not get. That
comes out of item 1, not out of more building.

Desktop is a shell over that screen — Tauri, so it is roughly a tenth of what
Electron would weigh — and it buys one thing the browser cannot: a window that is
already open when a spend needs answering.

Mobile is last on purpose. It is the only piece whose cost never ends: signing, store
review, a release process per platform, and a build that expires whether or not
anything changed. It is also the client most likely to be used to approve a spend
while distracted — which was the argument for building it after notifications rather
than before them. Notifications shipped, and they carry no figure precisely because of
this: a message you could approve from at a glance is the failure this item is exposed
to.

### 4. A stronger sandbox

`SANDBOX_BACKEND=docker` is a boundary and not a wall: it is a shared kernel, and the
socket core is handed is a socket that could start a privileged container if core were
persuaded to. This is the largest remaining risk in the system and it is ours now that
the sandbox is code in this repository.

Options, in the order they were originally weighed: E2B, Daytona, or a dedicated VPS
whose credentials reach nothing that matters. The port (`internal/sandbox`) takes a
third backend without touching the agent loop, which is most of why it is a port.

Until one of them, two rules stand: the one from
[architecture.md](architecture.md) — do not give core credentials you would not give a
contractor on their first day — and the one `internal/sandbox/local.go` states about
itself, which is that `local` is convenience and not isolation.

### 5. WhatsApp

The three platforms that shipped were the three whose access is a token an operator
issues to themselves. WhatsApp is not: the choice between the official Cloud API — a
business account, per-message pricing, template approval — and an unofficial library
that logs in as a person is an account-risk decision, not a coding one. Somebody has to
own that decision before there is anything to build, and the port takes a fourth adapter
without touching the inbound ladder.

### 6. More products, more machines

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
| **Native Windows desktop control** | [Phase 4](motivation.md#phases) says "evaluate only if a business tool requires it". Nothing has. Remote-controlling a desktop is a large attack surface bought for one tool's sake. |
| **A metric write API** | Definitions carry SQL and the names of variables holding database passwords. An endpoint that created one would be an endpoint that reads any database this service can reach. |
| **A tool-grant API** | A grant says what an agent may do and at what class. Grants live in `core/config/tools.yaml`, are read at startup and are snapshotted immutably when a run begins. An endpoint that wrote one would let a persuaded agent widen its own reach between two iterations of its own loop. |
| **A second spending gate in core** | A spending tool call files with the goal engine's ladder and waits. If core also decided, the weaker of the two gates would be the one that mattered, and it would be the one inside the process running model output. |
| **An audit mutation path** | Not "admin-only" — no method at all, in either service. A log with a delete path is a log somebody deletes, and that applies to a run's transcript as much as to the audit table. |
| **Agent-facing configuration** | Every limit an agent operates under lives in a file the agent cannot read or write, changed by a restart. A limit that can change without a deploy is a limit that changes quietly. |
| **A recipient field on a notification** | The engine says what happened; core sends it to the chat accounts `CORE_UNATTENDED_OWNER` has linked, and that is the only "who" there is. A field naming a target would let anything holding a bot key send a message from your instance to any chat account linked to it — and it would need a second decision, about which of them the caller may name. The one who answers for the box is the one who hears about it. |
| **Figures in a notification** | No amount, no currency, no pace ratio; a test asserts a headline holds no digit at all. A chat message is retained on somebody else's servers, and this project's posture is that the numbers stay on the box. The stronger reason is the second one: a message complete enough to decide from is a message people decide from at a glance, which is the failure mode the mobile item is most exposed to. The link is what gets you to the console. |
| **A notification history** | A notification is a message that was sent, not a record. The audit log already says a trigger fired and an approval is pending; a second list of the same events, readable with a bot key and scoped to no account, would be a second answer to the same question — and the two would disagree the first time a delivery failed. |
| **An operator key in the console's environment** | The console runs at bot level. An operator key sitting in a web server's environment is an operator key any request-handling bug can spend; an operator pastes theirs, it is verified before it is held, and it lapses on a timer. |
| **A pass-through proxy in the console** | It forwards a fixed list of 36 routes, each with the authority it needs decided in the module. A generic `/api/*` that took a path from a caller would make every route on both services reachable with whichever credential the console holds. |
| **A task queue in the goal engine** | The monitor is a timer over a bounded batch. A queue adds a moving part to the one layer whose job is to be predictable. |
| **Docker inside core's image** | The sandbox needs a docker *client*; a daemon in the same image means a privileged container, which is a worse boundary than the socket the compose file mounts. CI asserts the daemon is absent. |
| **Auto-raising caps** | The system will never widen its own budget in response to hitting it. Ever. |
| **Multi-tenancy** | Self-hosted, single-operator. Core has accounts, and a person's chats, runs, workspace and token budget are their own — but goals, money and the brakes belong to whoever runs the instance. `product` is a label on a goal, not a tenant boundary, and pretending otherwise would make the audit trail a lie. |
| **A hosted version** | Somebody else's box holding your audit log and your database credentials defeats the point. |

## Contributing to any of this

The pilot (item 1) is the one that unblocks everything else, and it needs an
operator with a real metric, not a contributor with a patch.

For code: integration tests (item 2) are the highest-value thing a newcomer can
pick up, and the hardest to get wrong. See [CONTRIBUTING.md](../CONTRIBUTING.md).

If you want to propose something from the absent list, open an issue with the
failure it prevents. "A missing limit means ask a human" is the kind of decision
that took an argument to arrive at, and it should take one to reverse.
