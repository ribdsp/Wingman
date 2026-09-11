# Forking and rebranding core

Wingman core is a fork of [Rakazo](https://github.com/elie222/rakazo), Apache-2.0.
This document is what you may change, what you must not, and how to keep the fork
mergeable for longer than a month.

- [Why a fork and not a dependency](#why-a-fork-and-not-a-dependency)
- [What the licence requires](#what-the-licence-requires)
- [Provisioning it](#provisioning-it)
- [Setting up remotes](#setting-up-remotes)
- [Rebranding](#rebranding)
- [Keeping the diff small](#keeping-the-diff-small)
- [Wiring it to the goal engine](#wiring-it-to-the-goal-engine)
- [Merging upstream](#merging-upstream)
- [If the fork drifts anyway](#if-the-fork-drifts-anyway)

## Why a fork and not a dependency

Rakazo is an application, not a library. There is no package to depend on: the
channels, the agent loop, the tool registry and the sandbox are the product. To
change its branding and add an API route, you change its source.

So `core/` is a **separate git repository** with its own history, gitignored by
this one. This repo does not vendor it, does not submodule it, and does not carry
its commits. `scripts/fork-core.sh` provisions it and writes down exactly which
commit you got.

That separation is what keeps `git merge upstream` a routine operation. Everything
this project *adds* — goals, monitoring, the spending gate, the audit trail — lives
in `goal-engine/`, in a different language, in a different process. Upstream never
touches those files, so there is nothing there to conflict.

## What the licence requires

Apache-2.0 lets you fork, modify, rebrand, self-host, and redistribute. Section 4
is the part with obligations, and they are light:

**You must keep:**

- `LICENSE` — upstream's Apache-2.0 text, unmodified, in `core/`.
- Existing copyright, patent, trademark and attribution notices in the source
  files you keep. Do not strip a header while renaming a component.
- `NOTICE`, if upstream ships one, with its contents intact. You may add your own
  attributions alongside; you may not remove theirs.

**You must state your changes.** A prominent note that files were modified. This
repo's [NOTICE](../NOTICE) does that at the top level; if you redistribute your
fork of core, put an equivalent note in `core/`.

**You may:**

- Rename the product, replace the logo, rewrite every UI string.
- Add, remove, and rewrite code.
- Self-host it, sell access to it, keep your changes private. Apache-2.0 has no
  copyleft — you are not obliged to publish your fork. (Wingman publishes anyway;
  that was the point of the project.)

**Trademarks are not licensed.** Section 3 grants patent rights, not trademark
rights. "Rakazo" is somebody else's name. You may say your software is *derived
from* Rakazo — that is a factual statement about lineage, and it belongs in your
NOTICE. You may not name your product something that suggests it *is* Rakazo, or
endorsed by it. The same applies to "Grok": describing Wingman as a Grok Bot
alternative is a comparison, not a claim of affiliation.

None of the above is legal advice. It is a reading of a short, well-understood
licence, and if you are redistributing commercially you should have your own
lawyer read it too.

## Provisioning it

```bash
# Fork elie222/rakazo on GitHub first, then:
./scripts/fork-core.sh --repo <you>/wingman-core --ref v1.0.0
```

| Flag | Default | |
|---|---|---|
| `--repo` | `elie222/rakazo` | Your fork. Warns if you clone upstream directly. |
| `--ref` | `main` | Tag, branch, or commit SHA. Warns if you track `main`. |
| `--dir` | `core` | Where it lands. Gitignored. |
| `--https` | off | HTTPS instead of SSH. |

**Pin a tag.** Tracking `main` means an upstream commit can change your agent
runtime between two deploys — and the goal engine will keep dispatching tasks into
whatever is there. The script warns about this; the warning is the point.

The clone is shallow. It is a deployment artifact, not a place to do upstream
archaeology. `git -C core fetch --unshallow` when you need the history — and you
will, the first time you merge.

Afterwards, `core/.wingman-core-ref` records the repo, the ref, the resolved
commit, and when. A tag can be moved; a SHA cannot. When something behaves oddly,
that file answers "which core is actually running".

The script refuses to overwrite an existing `core/`. It may hold your rebrand, and
re-cloning would throw away work that is in no history this repo can recover.

## Setting up remotes

Immediately after provisioning:

```bash
cd core
git remote rename origin fork                     # your fork
git remote add upstream https://github.com/elie222/rakazo.git
git fetch --unshallow upstream                    # you need history to merge
git remote -v
```

Naming your own fork `fork` rather than `origin` is worth the small friction. It
makes `git push` ambiguous and therefore deliberate, which is what you want when
one of the two remotes is somebody else's project.

## Rebranding

Do this in **one commit, on one branch**, separate from every functional change.
When a merge conflicts, you want to be able to tell a branding conflict from a
logic conflict at a glance.

```bash
git checkout -b rebrand
```

### Find every mention

```bash
grep -ril 'rakazo' . --exclude-dir=.git --exclude-dir=node_modules \
  | grep -v -e LICENSE -e NOTICE
```

Then read the list before you change anything. Occurrences fall into four groups,
and only one is safe to bulk-replace:

| | Do |
|---|---|
| **User-visible strings** — page titles, email subjects, onboarding copy, bot display names | Replace. This is the rebrand. |
| **Identifiers** — package names, env prefixes, DB names, class names, CSS classes | Mostly leave. Every one you rename is a permanent merge conflict, for zero user benefit. |
| **Attribution** — `LICENSE`, `NOTICE`, copyright headers, the repo URL in `package.json` | Leave. Renaming these is the one thing the licence forbids. |
| **External references** — links to upstream docs, issue templates pointing at upstream's tracker | Update, so your users file bugs with you rather than with upstream. |

A blind `sed -i s/rakazo/wingman/g` across the tree hits all four groups. It will
strip a copyright header, rename a database, and guarantee a conflict in every file
upstream ever touches again. Do not.

### A minimal rebrand

Enough to be a different product, small enough to merge:

- [ ] Product name in the app config / `.env.example` (`APP_NAME` or equivalent)
- [ ] `<title>`, favicon, logo assets, OG image
- [ ] Onboarding and marketing copy
- [ ] Email templates: sender name, subjects, footer
- [ ] Bot display name and avatar
- [ ] `README.md` — describe your fork, link upstream as the origin
- [ ] Issue templates and support links → your tracker
- [ ] Add a `NOTICE` stating that files were modified, if you redistribute

Deliberately **not** on that list: package names, environment variable prefixes,
database names, table names, internal module paths, CSS class names. Each is a
recurring merge cost with no visible payoff.

```bash
git commit -am 'chore: rebrand to Wingman'
git push -u fork rebrand
```

Keep this branch alive. Every upstream merge lands on it, and its diff against
upstream stays readable for years if you never mix logic into it.

## Keeping the diff small

The single best predictor of whether a fork survives is how many upstream files it
touches. Two habits:

**Add files rather than edit them.** A new route file, a new adapter, a new
component that the router points at — a merge conflict in a file upstream has never
seen is impossible.

**Put your logic outside core.** That is what `goal-engine/` is. Anything you are
tempted to add to core's business logic, ask whether it could be an HTTP call to
the goal engine instead. Usually it can, and then upstream can rewrite that whole
subsystem without touching your feature.

The goal engine has no equivalent of these rules because nobody upstream is
editing it. That asymmetry is the design.

## Wiring it to the goal engine

Core needs exactly one thing that upstream may not already have: a route that
accepts a task from an authenticated service.

```
POST /api/v1/tasks
Authorization: Bearer <WINGMAN_CORE_API_KEY>

{
  "botId": "growth",
  "channelId": "C0123",
  "brief": "MRR is 12% behind the pace needed for \"kejar MRR 50 juta …\"",
  "idempotencyKey": "goal:0f8c…:2026-09-11T18:00:00Z",
  "metadata": { "goalId": "0f8c…", "metricKey": "business.mrr", "product": "acme" }
}
```

Implement it as a **new file**, not an edit to an existing controller. Four things
it must do:

1. **Authenticate.** A shared secret is fine; an unauthenticated task-creation
   route is a way for anyone who can reach the port to make your agents work for
   them.
2. **Honour `idempotencyKey`.** The goal engine retries when a response is lost. A
   duplicate key must return the original task, not start a second one.
3. **Treat `metadata` as context, not authority.** Every policy decision was made
   in the goal engine. Nothing in that object should widen what the agent may do.
4. **Return quickly.** The engine's timeout is `WINGMAN_CORE_TIMEOUT` (30s). Queue
   the work and answer; do not run the agent inside the request.

If your fork's route lives somewhere else, set `WINGMAN_CORE_TASK_PATH` rather than
forking the goal engine to match. That variable exists precisely because core is a
fork and yours will differ.

Nothing else crosses the boundary. Core does not call the goal engine. If you want
an agent inside core to state a goal or request a spend, give it a **bot key** and
let it call the API like any other client — same routes, same limits, same audit
trail.

## Merging upstream

```bash
cd core
git fetch upstream --tags
git log --oneline HEAD..upstream/v1.1.0        # read this first
git checkout rebrand
git merge v1.1.0
```

Read the log before you merge, every time. You are pulling code that will run
agents against your business with your credentials; "it merged cleanly" is not a
review.

Pay attention to changes in:

- **The task/queue layer** — where your route plugged in.
- **Auth and API middleware** — whether your route is still authenticated the way
  you think.
- **The sandbox** — what agent-authored code can reach. This is the one that
  matters, because it is the boundary the goal engine cannot help with.
- **Migrations** — core's database, not yours. The goal engine's schema is
  untouched by any core upgrade, which is why the audit log lives there.

Then:

```bash
docker compose up -d --build          # in core's own stack
```

**Engage the kill switch before an upgrade** if agents may be mid-task. A task
dispatched into a service that restarts underneath it is a task nobody finishes,
and the goal engine will correctly notice the goal is still behind and trigger
again.

```bash
curl -s -XPUT localhost:8080/v1/flags/kill-switch \
  -H "Authorization: Bearer $OP" -H 'Content-Type: application/json' \
  -d '{"engaged":true,"reason":"core upgrade to v1.1.0"}'
```

Release it once core is healthy and you have watched one clean tick.

## If the fork drifts anyway

It happens. Upstream restructures, your rebrand branch stops merging, and every
attempt is an afternoon.

The exit is not to keep fighting the merge. It is to re-fork:

```bash
mv core core.old
./scripts/fork-core.sh --repo <you>/wingman-core --ref v2.0.0
# replay the rebrand checklist onto the new tree, by hand
diff -ru core.old core | less      # to find what you had actually changed
```

This is survivable *only* because your logic is not in there. The rebrand is a
checklist and one route is one file; both are an afternoon to redo. If you had put
the goal registry and the approval gate inside core, re-forking would mean
rewriting the product.

That is the whole argument for two services, and this is the paragraph where it
pays for itself.
