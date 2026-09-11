# Contributing

Thank you for looking. A few things worth knowing before you spend an evening on a
patch.

- [What this project wants](#what-this-project-wants)
- [Getting set up](#getting-set-up)
- [The layering rules](#the-layering-rules)
- [Tests](#tests)
- [Style](#style)
- [Commits and pull requests](#commits-and-pull-requests)
- [Review](#review)
- [Changing a safety default](#changing-a-safety-default)
- [Security issues](#security-issues)

## What this project wants

Wingman's job is to let an agent act on a business without being asked, and to make
the ways that goes wrong survivable. The interesting constraints are all in the
second half of that sentence.

Most valuable contributions, roughly in order:

1. **A pilot report.** You ran it against a real metric for a month; here is what
   the decisions looked like and where the defaults were wrong. This is worth more
   than any patch, and [docs/roadmap.md](docs/roadmap.md) explains why.
2. **Integration tests against a real Postgres.** The repositories are tested
   against a mock driver, which proves the SQL is shaped right, not that it is
   right.
3. **Bug reports with a failing test.** Anything in `internal/domain` can be
   reproduced in a unit test with no database at all, so a report there can be
   exact.
4. **Documentation that corrects the code's behaviour.** If a doc and the source
   disagree, that is a bug in one of them and finding it is a real contribution.

Less useful: new features in the goal engine. It is deliberately small, and
[the absent list](docs/roadmap.md#deliberately-absent) is a set of decisions rather
than a backlog.

## Getting set up

```bash
git clone https://github.com/ribdsp/wingman.git
cd wingman/goal-engine

go mod download
make test          # or: go test ./...
```

Go 1.26 or newer. Nothing else is required for the tests — no database, no Docker,
no network. That is on purpose: a test suite you can run on a plane gets run.

To run the service you need a Postgres:

```bash
cp .env.example .env
$EDITOR .env                # DATABASE_URL, GOAL_ENGINE_API_KEYS
make run
```

`make` with no target lists everything.

| | |
|---|---|
| `make test` | The suite. |
| `make test-race` | Under the race detector. Needs cgo and a C toolchain; CI runs it on every push, so nothing merges unraced. |
| `make cover` | Coverage, with the total printed. |
| `make fmt vet lint` | Formatting, vet, and golangci-lint if you have it. |
| `make build` | A stamped static binary into `bin/`. |

On Windows, `make` is often absent and every target is one `go` command — read the
Makefile and run the line you need.

## The layering rules

Two rules explain most of the structure, and a patch that breaks either will be
sent back:

**`internal/domain` is pure.** It decides whether a goal is behind pace, what a
spend request gets, and what a brief says. It takes values and returns values: no
database handle, no `time.Now()`, no HTTP client, no logger. `now` is a parameter.

This is why the evaluator can be tested against two dozen hand-written scenarios in
milliseconds, and why "what would this goal do tomorrow" is a function call. If you
find yourself wanting a clock in there, pass the time in.

**Services own the I/O, behind small ports.** Every dependency a service has is an
interface in `internal/service/ports.go` — `GoalStore`, `SampleReader`, `AuditSink`,
`MetricLookup`, `TaskCreator`, `Clock` — mostly one or two methods each. The
repositories satisfy them, and so do the fakes in the tests.

```
cmd/goal-engine        process: config, wiring, workers, shutdown
internal/handler       HTTP: bind, render, status codes
internal/middleware    request id, recover, access log, rate limit, auth
internal/service       orchestration, authorisation, audit writes
internal/repository    SQL. One type per table, parameterised
internal/domain        decisions. No I/O
```

Nothing in `internal/service` imports `gin`. Nothing in `internal/handler` writes
SQL. If your change needs to break that, say why in the pull request — it is a
design conversation, not a lint rule.

Two more, smaller:

- **`Actor` is an explicit parameter**, never smuggled through `context.Context`.
  `{Type, ID, RequestID}` is passed down from the handler, because a caller's
  identity is an argument to a decision and every audit row is written from it.
- **Errors go up, not into a log-and-continue.** The one exception is the audit
  write, which logs loudly rather than rolling back the action it was recording;
  that trade-off is documented where it happens.

## Tests

The bar is 80%. The suite is at 93.7% and the packages that make decisions are
higher than that:

```
internal/middleware   100.0%
internal/utils        100.0%
internal/domain        98.7%
internal/service       98.4%
internal/config        96.9%
internal/policy        95.7%
internal/core          95.7%
internal/handler       90.0%
internal/metrics       88.6%
internal/repository    88.3%
```

Write the test first. Not as ceremony — for this codebase specifically, the
evaluator's ladder has ten branches whose *order* is the safety model, and the only
way to be sure a change did not reorder them is a test per branch that existed
before the change.

Arrange–Act–Assert, and name the behaviour rather than the function:

```go
func TestEvaluate_behindPaceInsideCooldown_doesNotTrigger(t *testing.T) {
    // Arrange
    goal := activeGoal(t, withCooldown(6*time.Hour), withLastTrigger(now.Add(-2*time.Hour)))
    sample := sampleAt(now, 36_000_000)

    // Act
    got := domain.Evaluate(goal, sample, now)

    // Assert
    if got.Decision != domain.DecisionCooldownSkipped {
        t.Fatalf("decision = %s, want cooldown_skipped", got.Decision)
    }
}
```

Table-driven where the cases are genuinely uniform, separate functions where each
case needs its own setup. Repositories are tested with `go-sqlmock`; services with
the fakes in `fakes_test.go`. Do not add a test that needs a live Postgres to the
unit suite — that is item 2 on the roadmap and it belongs behind a build tag.

## Style

`gofmt` decides formatting; there is nothing to discuss. Beyond that, this codebase
has some habits:

- **Files stay small.** 200–400 lines is typical, 800 is the ceiling. Split by what
  the code does, not by what type it is.
- **Functions stay small.** Under 50 lines, early returns rather than nesting.
- **Comments say why, not what.** `// bind the request` is noise. The comment
  explaining why a cap denies instead of escalating is the most valuable line in
  that file.
- **JSON is camelCase, database tags are snake_case.** Mapping happens at the
  repository boundary, and snake_case never reaches the public API:

  ```go
  IsActive  bool      `db:"is_active" json:"isActive"`
  CreatedAt time.Time `db:"created_at" json:"createdAt"`
  ```

- **No magic numbers.** Named constants for thresholds, timeouts and limits.
- **Immutable by default.** An evaluation is an audit record; build a new one rather
  than mutating one in place.
- **Errors are wrapped with context and never swallowed.** A 500 body carries no
  driver message, DSN, query or host name — `requestId` plus the service log is the
  way in.

## Commits and pull requests

Conventional commits:

```
feat: add per-bot trigger budget
fix: reject a pushed sample for a sql metric
docs: correct the approval ladder in api.md
test: cover a zero-length goal period
refactor: extract the pace calculation
chore: pin the postgres image to 17-alpine
```

One logical change per commit. A pull request should say what it changes, why, and
what you ran. If it touches a decision path, say which test proves the order is
unchanged.

Before you open one:

```bash
make fmt vet test
```

`make test-race` too, if you have a C toolchain. CI runs it either way.

## Review

Every change gets read. Findings come back at four levels:

| | |
|---|---|
| **CRITICAL** | A security hole or data-loss risk. Blocks the merge. |
| **HIGH** | A bug, or something that will become one. Fix before merge. |
| **MEDIUM** | Maintainability. Fix if you can. |
| **LOW** | Style, taste, a suggestion. Yours to take or leave. |

Anything touching authentication, authorisation, the approval ladder, the kill
switch, the audit log, or SQL construction gets a security-focused pass as well.
That is not a comment on the contributor; it is where the consequences are.

## Changing a safety default

Some values in this codebase are arguments that have already been had. Changing one
is welcome, and it needs the argument rather than a diff:

- The default cooldown (6h) and its floor (15m)
- The default trigger budget (5 per period)
- The tolerance default (0.05) and its ceiling (0.5)
- Sample staleness (`METRIC_MAX_SAMPLE_AGE`, 26h)
- Approval TTL (24h)
- **The order of the evaluation ladder**
- **The order of the approval ladder**
- That a missing policy means "ask a human"
- That caps deny rather than escalate
- That anyone may engage the kill switch and only an operator may release it
- That an unreadable kill-switch flag means halt

For the last five, "it is inconvenient" is not an argument — inconvenience is what
they are for. What would move them is a failure mode they cause that is worse than
the one they prevent. If you have one, that is a genuinely interesting issue and it
should be opened as one.

Everything above is documented in [docs/goal-engine.md](docs/goal-engine.md) with
the reasoning attached. If you disagree with the reasoning, argue with that
paragraph; it is there to be argued with.

## Security issues

Do not open a public issue. See [SECURITY.md](SECURITY.md).

## Licence

Contributions are under Apache-2.0, the same as the project. There is no CLA. Keep
upstream Rakazo's copyright headers intact in `core/` — see
[docs/fork-and-rebrand.md](docs/fork-and-rebrand.md).

By opening a pull request you confirm you wrote the change or have the right to
submit it, and that it may be released under Apache-2.0.
