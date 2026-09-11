# CLAUDE.md

Project-specific guidance for Claude Code working in this repository.

## What this is

Wingman: a self-hosted assistant that acts on a business without being asked. Two
services.

| | |
|---|---|
| `goal-engine/` | Go. **This repo owns it.** Goals, metric monitoring, the trigger bridge, the spending gate, the audit log. |
| `core/` | A fork of [Rakazo](https://github.com/elie222/rakazo), TypeScript, **gitignored, not in this history**. Provisioned by `scripts/fork-core.sh`. Do not edit it as part of a change to this repo. |

Read [docs/motivation.md](docs/motivation.md) for intent,
[docs/architecture.md](docs/architecture.md) for structure,
[docs/goal-engine.md](docs/goal-engine.md) for the decision model.

`PRD.md` may exist in the working tree. It is **gitignored** — it names products,
people and infrastructure that are not public. `docs/motivation.md` is its redacted
public equivalent and the one to keep in step with the code. Do not quote PRD.md
into a tracked file, and do not un-ignore it.

## Commands

From `goal-engine/`:

```bash
go test ./...                    # the whole suite; no database, no network needed
go build ./...
gofmt -l ./cmd/ ./internal/      # must print nothing
go vet ./...
go test -coverprofile=coverage.out -covermode=atomic ./...
go tool cover -func=coverage.out | tail -1
```

The Makefile wraps these, but **`make` is not installed on this machine** — read the
target and run the `go` line directly.

**Docker is usually not running here.** The Dockerfile and
`deploy/docker-compose.yml` are syntax-verified, not executed. Do not claim a
container works.

**`-race` needs cgo and gcc, which are absent here.** It runs in CI only. Do not
report a race-clean run you did not perform.

## Layering — enforced, not aspirational

```
cmd/goal-engine        process: config, wiring, workers, shutdown
internal/handler       HTTP: bind, render, status codes          ← gin lives here
internal/middleware    request id, recover, access log, rate limit, auth
internal/service       orchestration, authorisation, audit writes
internal/repository    SQL. One type per table, parameterised
internal/database      pool, migrations
internal/domain        decisions. No I/O, no clock, no database
```

Plus `internal/metrics` (registry + samplers), `internal/policy` (spending policy
registry), `internal/core` (client for core), `internal/utils` (response envelope).

Three rules that a change must not break:

1. **`internal/domain` is pure.** Values in, values out. No `time.Now()` — `now` is
   a parameter. No database handle, no HTTP client, no logger. This is why the
   evaluator is testable in milliseconds.
2. **Services depend on interfaces in `internal/service/ports.go`**, not on
   repositories. Most ports have one or two methods. Repositories satisfy them; so
   do the fakes in `fakes_test.go`.
3. **`Actor{Type, ID, RequestID}` is an explicit parameter.** Never smuggled through
   `context.Context`. Every audit row is written from it.

Nothing in `internal/service` imports `gin`. Nothing in `internal/handler` writes
SQL.

## Conventions

- **API JSON is camelCase. `db:` tags are snake_case.** Map at the repository
  boundary; snake_case never reaches the public API.
- **Shared envelope** for every response (`success/code/message/data/error/meta`) via
  `internal/utils`. Do not invent a second shape.
- **Nine stable error codes.** Listed in [docs/api.md](docs/api.md#errors). Adding a
  tenth is an API change, not a detail.
- Files 200–400 lines, 800 max. Functions under 50 lines. Early returns over
  nesting.
- Comments explain **why**. The line explaining why a cap denies instead of
  escalating is worth more than any `// bind the request`.
- Immutable by default — an evaluation is an audit record, build a new one.
- Errors wrapped with context, never swallowed. A 500 body carries no driver
  message, DSN, query or host name.

## Safety invariants — do not "simplify" these

Each is enforced in code and documented with its reasoning. Changing one needs an
argument in the PR, not a diff.

- Privilege is fixed at startup by which env var listed the key
  (`GOAL_ENGINE_API_KEYS` = operator, `GOAL_ENGINE_BOT_KEYS` = bot). No request field
  can raise a role.
- Operator-only operations are checked **twice**: route middleware *and* the service.
  This is not redundancy to clean up.
- `PUT /v1/flags/kill-switch` is deliberately **not** route-gated — anyone may
  engage, only an operator may release, enforced in the service. Do not add
  `RequireOperator` to that route.
- An unreadable kill-switch flag means **engaged**. Unreadable means halt.
- A missing spending policy means **`pending`** (ask a human), never auto-approve.
  `config/policies.yaml` ships empty on purpose.
- Caps (`hardCap`, `dailyCap`) **deny**; they do not escalate to a human.
- `enabled: false` **denies**.
- Metric definitions exist only in operator-owned YAML. **Never add an API that
  creates one** — they carry SQL and the names of env vars holding DB passwords.
- The audit log has **no** update or delete method. Not "admin-only" — none.
- Goal targets are operator-only to edit. An agent must not be able to lower the
  bar.
- Trigger cooldown and per-period budget have restrictive defaults and hard floors.
  Omitting them must never mean zero, because zero means "no limit" to the
  evaluator.
- The evaluation ladder's **order** is the safety model. Ten branches, tested one
  per branch. Reordering is a behaviour change.
- Sample staleness (`METRIC_MAX_SAMPLE_AGE`) stops evaluation rather than reading a
  dead feed's last number as on-track.
- Secrets are never logged, echoed or printed: not API keys, not DSNs (only
  datasource names), not the core API key.

## Tests

Bar is 80%; the suite is at 93.7% and decision code is higher. Write the test first
— for this codebase specifically, the only way to know a change did not reorder the
ladder is a test per branch that existed beforehand.

Arrange–Act–Assert. Name the behaviour, not the function:

```go
func TestEvaluate_behindPaceInsideCooldown_doesNotTrigger(t *testing.T)
```

Repositories use `go-sqlmock`; services use the fakes in `fakes_test.go`. **Do not
add a test that needs a live Postgres to the unit suite** — integration tests belong
behind a build tag (roadmap item 2).

## Docs must match the code

Every claim in `docs/` was verified against source. If you change behaviour, update
the doc in the same commit. The files that make specific claims:

| | Claims |
|---|---|
| `docs/api.md` | Every route, default, bound, error code, the approval ladder |
| `docs/goal-engine.md` | Pace maths, the ten-branch ladder, the approval ladder |
| `docs/architecture.md` | Layering, the core payload shape, table list |
| `docs/deployment.md` | Env vars, compose behaviour, troubleshooting |
| `goal-engine/config/policies.yaml` | The ladder, in comments an operator reads |

A doc and the code disagreeing is a bug in one of them.

## Do not

- Edit `core/` as part of a change here. It is a separate repository.
- Clone research repositories into this tree. Use TEMP.
- Add a dependency without a reason that survives "could this be 20 lines?".
- Commit or push unless asked.
- Create or push a GitHub repository, or run `gh repo fork`, without explicit
  approval — publishing is outward-facing.
- Put secrets in `.env.example` beyond `CHANGE_ME` placeholders.
- Claim something was tested that was not. Docker, `make` and `-race` are all
  unavailable locally.

## Repo layout

```
README.md                  what it is, quick start, safety summary
docs/                      motivation, architecture, goal-engine, api,
                           deployment, fork-and-rebrand, roadmap
deploy/docker-compose.yml  goal engine + its Postgres
scripts/fork-core.sh       provisions core/
goal-engine/               the Go service
  cmd/ internal/ migrations/ config/
  .env.example             the full env surface
PRD.md                     gitignored; the private, unredacted requirements
core/                      gitignored; a fork of Rakazo
```

## Language

The user writes Indonesian; reply in the language they use. Code, identifiers,
commit messages, API fields and documentation stay English. The private `PRD.md` is
Indonesian and stays that way; `docs/motivation.md`, being public, is English.
