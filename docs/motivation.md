# Why Wingman exists

The public version of this project's requirements. It is the reasoning the code was
built from, with one operator's product names and infrastructure details left out.

- [Background](#background)
- [The problem](#the-problem)
- [Goals](#goals)
- [Non-goals](#non-goals)
- [Who it is for](#who-it-is-for)
- [What Rakazo already gives you](#what-rakazo-already-gives-you)
- [What Wingman adds](#what-wingman-adds)
- [Requirements](#requirements)
- [Phases](#phases)
- [Risks](#risks)

## Background

Grok Bot introduced a pattern worth taking seriously: an AI agent with a *body* — a
persistent cloud computer, a browser, a desktop — that works in the background
rather than only answering when spoken to.

That pattern is interesting for one person running several products, because the
work that actually needs doing is not "answer my question". It is "notice that this
number is going the wrong way, and do something about it". A chat assistant cannot
do that, no matter how good the answers are, because nobody is asking.

[Rakazo](https://github.com/elie222/rakazo) (open source, Apache-2.0) already
provides most of the technical foundation: a web app, a desktop app, a mobile app,
a sandbox per bot, delegation between bots, and an approval workflow. Wingman is a
fork of Rakazo with a **goal-driven autonomous trigger layer** on top.

## The problem

An assistant that waits for a prompt is limited by how often you remember to
prompt it. The problems worth catching are exactly the ones you have not noticed
yet: revenue drifting behind where it needs to be, tickets piling up, signups
flattening two weeks before you would have looked.

Two things are missing from a chat assistant, and neither is intelligence:

1. **Something that watches.** A business metric, a target, a deadline, and a
   process that compares them on a timer.
2. **Something that makes acting safe.** An agent that can start work unprompted
   and spend money unprompted is not an assistant. It is an outage with a budget.

The second is the harder half, and it is where most of this project's code went.

## Goals

1. One self-hosted assistant for both personal and business work, on
   infrastructure the operator controls.
2. Agents that can plan and execute multi-step work in a sandbox, not just reply.
3. A goal registry: business targets stated in plain language, stored with the
   metric that measures them.
4. Monitoring that compares each goal against where it should be *by now*, and
   wakes an agent when it has fallen behind.
5. A mandatory approval gate for anything that costs money, with thresholds the
   operator sets.
6. A complete, append-only audit trail of what the agent did and who let it.
7. Separate contexts per product and for personal work, without cloning
   configuration from scratch each time.
8. A kill switch that anyone can reach and only a human can release.
9. **Release it free and open source**, because nothing about the design is a
   trade secret and the safety argument gets better with more readers.

## Non-goals

- **Phone and SMS channels.** Out of scope until the autonomy layer has been boring
  for several months.
- **Native Windows desktop control.** Evaluated only if a business tool genuinely
  requires it. Remote-controlling a desktop is a large attack surface to buy for one
  tool's sake.
- **Multi-tenancy.** Self-hosted, single-operator. Products are labels on goals, not
  tenant boundaries.
- **A hosted version.** Somebody else's box holding your audit log and your database
  credentials defeats the point.

## Who it is for

**Primary:** one operator running several products, who wants oversight of all of
them without checking each one by hand.

**Secondary:** anyone, because it is open source. If you run a small business and
want an agent that notices things, the goal engine does not care what your metrics
mean.

## What Rakazo already gives you

| | |
|---|---|
| Chat and channels | Web, desktop (Electron), mobile (Expo) |
| Agents with a computer | A sandbox per bot: shell, browser, files |
| Delegation | Bots handing work to other bots |
| Approval workflow | Built-in confirmation for sensitive tool use |
| Memory and tools | Per-bot configuration, tool registry |
| Self-hosting | Docker Compose, PostgreSQL, a worker queue |

Roughly 80% of what this project needed, already written and maintained by someone
else. Forking it rather than rebuilding it was the single largest decision here.

## What Wingman adds

| | |
|---|---|
| **Goal registry** | Active business targets — "10k weekly active users for product A" — with the metric that measures them and the period they run over |
| **Business metric monitor** | Reads each metric on a timer, from a push feed, a read-only database query, or a JSON endpoint |
| **Trigger bridge** | When a goal has fallen behind pace, creates a task in core. This is the piece that makes the agent act without being asked |
| **Approval threshold policy** | Per-action-type limits: auto-approve below a threshold, ask a human in the middle band, refuse above a cap |
| **Business audit log** | Append-only, every actor and every action, with no delete path |
| **Branding** | A rebrand of core, with upstream attribution intact |

All of it lives in a separate service, in a different language, with its own
database. [architecture.md](architecture.md) explains why that separation is the
design and not an accident.

## Requirements

Condensed to what the goal engine actually implements. The full behaviour is in
[goal-engine.md](goal-engine.md) and [api.md](api.md).

**Goals.** A goal is a target on one declared metric over one period, with a
baseline captured when it is created, a tolerance for being off pace, and limits on
how often falling behind may wake an agent. The operator's own wording is kept
verbatim, because it is what the agent is shown.

**Monitoring.** On a timer, each active goal is evaluated against where it should be
by now rather than against its target. Every evaluation is recorded, including the
ones that decided to do nothing.

**Triggering.** A goal that is behind pace, outside its cooldown, and inside its
trigger budget creates one task in core with a brief that quotes the original
wording and attaches the numbers. Nothing else crosses the boundary.

**Approvals.** Every action that costs money is decided against operator-owned
policy: auto-approved below a threshold, queued for a human in the middle, denied
above a cap. A missing policy means ask a human. Unanswered requests expire.

**Audit.** Every decision, dispatch, approval and configuration change is recorded
with its actor, its outcome and the request that caused it.

**Control.** A kill switch that halts every trigger and denies every spend; anyone
may engage it, only an operator may release it.

## Phases

**Phase 1 — deploy and validate.** Core on one VPS, rebranded. Test personal and
business chat, autonomous coding, and core's built-in approvals.

**Phase 2 — the goal-driven layer.** Goal registry, metric monitor and trigger
bridge for one pilot product. Validate end to end: a goal stated in chat, a gap
detected, a plan drafted, approval granted, action executed.

**Phase 3 — expansion.** The same layer for other products. Evaluate multi-host
deployment and a dedicated cloud sandbox if load demands it. Evaluate native desktop
control only if a business tool requires it.

[roadmap.md](roadmap.md) says honestly where this has and has not got to.

## Risks

| Risk | Mitigation |
|---|---|
| The agent executes something wrong or harmful | A mandatory approval gate for financial actions; a complete audit log; a kill switch |
| Token and compute costs balloon from autonomous work | Usage caps per bot and per period; a per-goal trigger cooldown and budget |
| Rakazo is young and moves fast | Pin a release when deploying; read the changelog before upgrading; keep all added logic outside the fork |
| A Linux sandbox is not enough for tools that need Windows | Accept the limitation early; evaluate remote Windows only if it becomes a real blocker |

The first row is the one that shaped the codebase. Almost every design decision in
[goal-engine.md](goal-engine.md) is an answer to "what if the agent is wrong about
this?" — and [SECURITY.md](../SECURITY.md) states plainly which of those answers
hold and which do not.
