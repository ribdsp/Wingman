# Wingman

A self-hosted assistant that acts on your business without being asked.

Most assistants wait for a prompt. Wingman watches the numbers you told it to care
about, notices when one of them has fallen behind, and wakes an agent about it —
then asks permission before anything costs money.

It is two Go services and a console, all self-hosted, all yours, all in this repository:

| | What it is |
|---|---|
| **core** | The agent runtime: the loop that plans and calls tools, the sandbox it runs code in, accounts, chats, the chat platforms you can reach it on, per-user token budgets. Go, in `core/`. |
| **goal-engine** | The part that makes it autonomous: goals, metric monitoring, the trigger bridge into core, the spending gate, and an append-only audit log. Go, in `goal-engine/`. |
| **web** | The screen you watch it on: the approval queue, every goal's pace, what the monitor just decided, the audit log, a chat, and the kill switch. Next.js, in `web/`. |

Two Go modules, two Postgres databases, deployed separately. They talk over HTTP and
neither trusts the other by default: core asks the engine for permission to spend, and
the engine asks core to do work. Nothing else crosses. The console holds no database of
its own — it renders what the two services say and forwards what you decide.

**Status: early.** Everything here is built and tested, and none of it has been running
in production long enough for anyone to make promises about it. Read
[docs/roadmap.md](docs/roadmap.md) before you plan around it.

## The idea

You say, once:

> kejar MRR 50 juta sebelum akhir kuartal

That becomes a goal: a metric (`business.mrr`), a target (50,000,000), a deadline,
and a tolerance for being off pace. Then, on a timer, without you:

1. The monitor reads the metric.
2. The evaluator compares it against where the goal *should* be by now — not
   against the target, against the pace line to the target.
3. If it is behind by more than the tolerance, the trigger bridge creates a task
   in core, and an agent starts working on it with your original wording quoted
   back to it.
4. If that work wants to spend money, it asks the approval gate first. Under your
   threshold it proceeds and is recorded. Over it, you get asked. Over your cap,
   it is refused.
5. Everything above is written to an audit log with no delete path.

That fourth step is the whole reason this project has an opinion. An agent that
can act unprompted and spend money unprompted is not an assistant, it is an
outage with a budget.

## Quick start

```bash
git clone https://github.com/ribdsp/wingman.git
cd wingman/deploy

# 1. Four environment files: the stack's, the engine's, core's, the console's. Each
#    service is handed only its own — the engine holds the keys that approve spending,
#    and the other two are the ones that ask.
cp .env.example                 .env
cp ../goal-engine/.env.example  .env.goal-engine
cp ../core/.env.example         .env.core
cp ../web/.env.example          .env.web
$EDITOR .env .env.goal-engine .env.core .env.web

# 2. The sandbox workspace, owned by the uid core runs as.
sudo mkdir -p /srv/wingman/workspaces && sudo chown 65532:65532 /srv/wingman/workspaces

# 3. Up.
docker compose up -d --build
curl -s localhost:8080/readyz    # the goal engine
curl -s localhost:8081/readyz    # core

# 4. Core's first account. Registration is closed by default, so this is the way in.
read -rs PW && printf '%s' "$PW" | docker compose exec -T core \
  /app/core createuser -email you@example.com -name 'Your Name'
```

Then sign in to the console at **http://127.0.0.1:3000** with that account. It is the
approval queue, every goal's pace, the last tick's decisions, the audit log and a chat
on one screen — plus the kill switch, which anyone signed in can throw and only an
operator can release.

An operator key is never in the console's environment; you paste yours when you need to
resolve an approval or move a target, and it is held encrypted in your own cookie for
thirty minutes. [docs/web.md](docs/web.md) is how that works and why.

Chat platforms are optional and off unless a token is set — with one, you link your
Telegram, Slack or Discord account with a single-use code and talk to it there. Every
connection is outbound, so nothing has to be exposed:
[docs/deployment.md](docs/deployment.md#chat-platforms-if-you-want-any).

With a platform linked, `NOTIFY_ENABLED=true` on the engine gets you a one-line message
when a goal falls behind and an agent is woken, or when a spend is waiting for you. It
carries a link to the console and no figures, for reasons worth reading before you turn
it on: [docs/deployment.md](docs/deployment.md#notifications-if-you-want-them).

Then state a goal:

```bash
curl -s localhost:8080/v1/goals \
  -H "Authorization: Bearer $OPERATOR_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
        "product": "acme",
        "title": "MRR to 50M by end of Q4",
        "sourceText": "kejar MRR 50 juta sebelum akhir kuartal",
        "metricKey": "business.mrr",
        "targetValue": 50000000,
        "periodEnd": "2026-12-31T23:59:59+07:00"
      }'
```

…report a value for it, and ask the engine to look now instead of waiting an hour:

```bash
curl -s localhost:8080/v1/metrics/business.mrr/samples \
  -H "Authorization: Bearer $OPERATOR_KEY" \
  -H 'Content-Type: application/json' -d '{"value": 31400000}'

curl -s -XPOST localhost:8080/v1/monitor/tick -H "Authorization: Bearer $OPERATOR_KEY"
```

`TRIGGER_DRY_RUN=true` is the default in `goal-engine/.env.example`. Goals are
evaluated, decisions are recorded, the audit log fills up — and no task reaches core.
Read a week of what the engine *wanted* to do before you let it do any of it.

Full walkthrough: [docs/deployment.md](docs/deployment.md).

## What keeps it safe

Not a promise that the agent behaves. A set of things it cannot do.

In the goal engine, which decides *whether* to act and *whether* to spend:

- **Nothing spends money by default.** `config/policies.yaml` ships empty, and an
  action type with no policy needs a human every time. A missing limit is never
  read as "no limit".
- **Caps refuse, they do not escalate.** Past a hard or daily cap the request is
  denied outright. Being asked to rubber-stamp a breach is how caps stop working.
- **The agent cannot move its own goalposts.** Editing a target is operator-only,
  enforced at the route and again in the service.
- **The agent cannot define its own metrics.** Metric definitions carry SQL and the
  names of environment variables holding database passwords. They live in
  operator-owned YAML with no API write path.
- **Anyone can hit the brakes; only a human can release them.** An agent that
  notices it is doing damage can engage the kill switch. Turning it off is yours.
  If the flag cannot be read at all, the engine halts.
- **Triggers are rate-limited by construction.** Every goal has a cooldown
  (6h default, 15m floor) and a trigger budget per period (5 default). Leaving
  them blank gets the restrictive default, not zero.
- **A stale metric stops evaluation.** A dead feed's last number would otherwise
  read as on-track forever.
- **A notification tells you to look, never what to decide.** Off by default. Turned on,
  it says a goal fell behind or a spend is waiting, with a link and no amount, currency or
  pace figure — a chat message lives on somebody else's servers, and a message complete
  enough to approve from is one people approve at a glance.
- **The audit log is append-only.** No service method updates or deletes a row.

In core, which decides *how far* one run may go:

- **An agent starts with no tools.** `config/tools.yaml` ships granting nothing, and a
  tool with no grant is denied. A fresh instance can think and answer; it cannot act
  until you say what it may do. There is no API that writes that file.
- **Money is never decided in core.** A tool call that spends files a request with the
  goal engine's ladder and waits. Core has no second, weaker gate.
- **Every run is bounded before it starts.** Iterations, tool calls, tokens per run,
  tokens per person per day, a timeout on each model call and each sandbox command.
  Omitting a limit gets the restrictive default; zero never means "no limit".
- **The kill switch stops runs too.** Checked before a run starts and between
  iterations, fail-closed — unreadable means engaged, engaged means halt.
- **Code runs in a throwaway container.** No network unless you grant it, no
  capabilities, read-only root, memory and pid limits, one workspace per person and
  never shared.
- **Registration is closed by default.** A self-hosted box found on the internet with
  open sign-up is a box running strangers' code in your sandbox on your API key.
- **A stranger in a chat app reaches one thing.** A sender nobody has linked gets the
  instructions for linking and nothing else — no run, no tokens spent. Linking needs a
  code minted from a signed-in session and sent over the channel, and shared rooms are
  off until you turn them on.
- **A run's transcript is append-only**, and a run that cannot read its own budget
  stops rather than guessing.
- **Nothing picks who gets messaged.** A notification goes to the chat accounts the
  unattended owner linked, resolved to their own DMs; the request has no recipient field,
  so nothing holding a bot key can make your instance message somebody else.

In the console, which decides nothing but is where the credentials would leak from:

- **The operator key is not in its environment.** The console runs on a bot key that can
  read everything and engage the kill switch, and nothing more. Releasing the switch,
  resolving an approval or moving a target needs a key an operator pastes, held encrypted
  in their own cookie for thirty minutes with the countdown on screen.
- **It cannot reach a route it does not list.** Both proxy endpoints check an explicit
  allowlist before any credential is chosen, so there is no path from the page to
  reporting a metric sample, running a tick, or dispatching a task.
- **Every read happens on the server.** No key, no session token and no service address
  reaches the browser, and the kill-switch reading fails closed there too.

Both ladders — the engine's ten evaluation branches, core's eleven stop reasons — are
tested one test per branch, because their *order* is the safety model and a reordering
is silent.

## Documentation

| | |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the two services fit, and why the goal engine is separate |
| [docs/goal-engine.md](docs/goal-engine.md) | Goals, pace, triggers, approvals — the model in detail |
| [docs/core.md](docs/core.md) | The agent loop, the stop-reason ladder, budgets, tools, sandboxes |
| [docs/web.md](docs/web.md) | The console: its screens, its three credentials, and what it cannot reach |
| [docs/api.md](docs/api.md) | Every endpoint on both services, every error code |
| [docs/deployment.md](docs/deployment.md) | Self-hosting on a VPS, from clone to first trigger |
| [docs/roadmap.md](docs/roadmap.md) | What exists, what is next, what is deliberately absent |
| [docs/motivation.md](docs/motivation.md) | Why this exists: the problem, the goals, the phases, the risks |
| [SECURITY.md](SECURITY.md) | Reporting a vulnerability, and the threat model |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Tests, style, and how a change gets reviewed |
| [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) | How disagreements here are expected to go |

## Licence

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

Both Go services are original work: an earlier plan had core start as a fork of
[Rakazo](https://github.com/elie222/rakazo) and it does not. The console's visual
language *is* adapted from Rakazo, which is Apache-2.0 like this project —
[NOTICE](NOTICE) says what was taken, which files it lives in, and what was changed.
The Go dependencies are listed in each module's `go.mod` and none are vendored; the
console's are in `web/package.json`. "Grok" and "Rakazo" are the marks of their
respective owners, referenced here only to describe prior art.
