# Wingman

A self-hosted assistant that acts on your business without being asked.

Most assistants wait for a prompt. Wingman watches the numbers you told it to care
about, notices when one of them has fallen behind, and wakes an agent about it —
then asks permission before anything costs money.

It is two services, both self-hosted, both yours:

| | What it is |
|---|---|
| **core** | The agent runtime: chat channels, tools, memory, the sandbox agents run code in. A fork of [Rakazo](https://github.com/elie222/rakazo) (Apache-2.0), provisioned by `scripts/fork-core.sh`. |
| **goal-engine** | The part that makes it autonomous: goals, metric monitoring, the trigger bridge into core, the spending gate, and an append-only audit log. Written for this project, in Go, in `goal-engine/`. |

**Status: early.** The goal engine is built and tested; core is a fork you
provision yourself. Nothing here has been running in production for long enough
for anyone to make promises about it. Read [docs/roadmap.md](docs/roadmap.md)
before you plan around it.

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
cd wingman

# 1. Bring in core (the Rakazo fork). Pin a release rather than tracking main.
./scripts/fork-core.sh --repo <you>/wingman-core --ref v1.0.0

# 2. Configure the goal engine.
cd deploy
cp ../goal-engine/.env.example .env
$EDITOR .env          # POSTGRES_PASSWORD, GOAL_ENGINE_API_KEYS, core URL + key

# 3. Up.
docker compose up -d --build
curl -s localhost:8080/readyz
```

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

`TRIGGER_DRY_RUN=true` is the default in `.env.example`. Goals are evaluated,
decisions are recorded, the audit log fills up — and no task reaches core. Read a
week of what the engine *wanted* to do before you let it do any of it.

Full walkthrough: [docs/deployment.md](docs/deployment.md).

## What keeps it safe

Not a promise that the agent behaves. A set of things it cannot do.

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
- **The audit log is append-only.** No service method updates or deletes a row.

## Documentation

| | |
|---|---|
| [docs/architecture.md](docs/architecture.md) | How the two services fit, and why the goal engine is separate |
| [docs/goal-engine.md](docs/goal-engine.md) | Goals, pace, triggers, approvals — the model in detail |
| [docs/api.md](docs/api.md) | Every endpoint, every error code |
| [docs/deployment.md](docs/deployment.md) | Self-hosting on a VPS, from clone to first trigger |
| [docs/fork-and-rebrand.md](docs/fork-and-rebrand.md) | Provisioning core and rebranding it without breaking attribution |
| [docs/roadmap.md](docs/roadmap.md) | What exists, what is next, what is deliberately absent |
| [docs/motivation.md](docs/motivation.md) | Why this exists: the problem, the goals, the phases, the risks |
| [SECURITY.md](SECURITY.md) | Reporting a vulnerability, and the threat model |
| [CONTRIBUTING.md](CONTRIBUTING.md) | Tests, style, and how a change gets reviewed |
| [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) | How disagreements here are expected to go |

## Licence

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).

Wingman's core component is a derivative work of
[Rakazo](https://github.com/elie222/rakazo) and carries its copyright notices;
the goal engine is original to this project. "Grok" and "Rakazo" are the marks of
their respective owners, referenced only to describe lineage and interoperability.
