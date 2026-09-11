<!--
Thanks for the patch. Keep this short — the point of the questions is to save a
review round trip, not to make you write an essay.
-->

## What this changes

<!-- One or two sentences. -->

## Why

<!-- The problem, or a link to the issue. -->

## What you ran

<!--
From goal-engine/. Tick what you actually ran; CI runs the rest.
-->

- [ ] `go test ./...`
- [ ] `gofmt -l ./cmd/ ./internal/` prints nothing
- [ ] `go vet ./...`
- [ ] `go test -race ./...` (needs cgo and a C toolchain — CI covers it if you cannot)
- [ ] Ran it against a real Postgres

## Behaviour

- [ ] No behaviour change (refactor, docs, tests, tooling)
- [ ] Behaviour changes, and the tests that prove it are in this PR

<!--
If a decision path changed — the evaluation ladder, the approval ladder,
authorisation, the kill switch — say which test proves the order is what you
intended. The order of those ladders *is* the safety model.
-->

## Does this touch anything in the safety set

<!--
Ticking a box is not a problem. It means the PR needs a paragraph of reasoning
rather than just a diff. See CONTRIBUTING.md § Changing a safety default.
-->

- [ ] An agent could do something with this that it currently cannot
- [ ] A default or floor changed (cooldown, tolerance, trigger budget, TTL, staleness)
- [ ] The order of a decision ladder changed
- [ ] Authorisation, the two privilege levels, or the kill switch
- [ ] The audit log
- [ ] What happens when something cannot be read
- [ ] None of the above

## Docs

- [ ] No doc claims anything different now
- [ ] Updated in this PR: <!-- which files -->

<!--
docs/api.md, docs/goal-engine.md, docs/architecture.md, docs/deployment.md and
goal-engine/config/policies.yaml all make specific claims about behaviour. A doc
and the code disagreeing is a bug in one of them.
-->
