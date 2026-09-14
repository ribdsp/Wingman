# Why Wingman exists

The public version of this project's requirements. It is the reasoning the code was
built from, with one operator's product names and infrastructure details left out.

- [Background](#background)
- [The problem](#the-problem)
- [Goals](#goals)
- [Non-goals](#non-goals)
- [Who it is for](#who-it-is-for)
- [Why it is two services](#why-it-is-two-services)
- [What Wingman is made of](#what-wingman-is-made-of)
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

Wingman is that pattern, self-hosted, with the autonomy made survivable: a
**goal-driven autonomous trigger layer** over an agent runtime, both written for this
project.

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
- **Multi-tenancy.** Self-hosted, single-operator. Core has accounts, and a person's
  chats, runs, workspace and token budget are their own — but that is multi-*user*, not
  multi-tenant. Goals, spending policy, tool grants and the kill switch belong to
  whoever runs the instance, and products are labels on goals rather than tenant
  boundaries.
- **A hosted version.** Somebody else's box holding your audit log and your database
  credentials defeats the point.

## Who it is for

**Primary:** one operator running several products, who wants oversight of all of
them without checking each one by hand.

**Secondary:** anyone, because it is open source. If you run a small business and
want an agent that notices things, the goal engine does not care what your metrics
mean.

## Why it is two services

The obvious build is one service. Wingman is two, and the split is the design rather
than an accident of history.

| | |
|---|---|
| **core** | The agent runtime. The loop that plans and calls tools, the sandbox it runs code in, accounts, chats, the chat platforms it can be reached on, per-person token budgets. Decides **how far** one run may go. |
| **goal-engine** | Goals, the metric monitor, the trigger bridge, the spending gate, the audit log. Decides **whether** to act at all, and **whether** money may move. |

Core is what makes Wingman useful. The goal engine is what makes it safe to leave
running, and it is where most of this project's code went.

Keeping them apart means the code that executes agent-authored output does not share a
process, a database or a credential with the code that authorises spending. Core asks
the engine for permission and reports what it used; the engine asks core to do work.
Nothing else crosses. [architecture.md](architecture.md) has the boundary in detail.

An earlier plan had core start as a fork of
[Rakazo](https://github.com/elie222/rakazo) rather than being written here. Three things
changed it: Rakazo was four weeks old and pushed daily, which is not a foundation to
build an autonomy layer on top of; the expensive half of it — a per-vendor integration
marketplace and a web app — was not the half needed; and supporting MCP makes every MCP
server in existence a tool source through one protocol, which is what that marketplace
was for. So core is Go, in this repository, with the same layering, testing discipline
and safety posture the goal engine already proves out. One language, one review
standard, one CI, and no upstream to merge.

## What Wingman is made of

| | |
|---|---|
| **Goal registry** | Active business targets — "10k weekly active users for product A" — with the metric that measures them and the period they run over |
| **Business metric monitor** | Reads each metric on a timer, from a push feed, a read-only database query, or a JSON endpoint |
| **Trigger bridge** | When a goal has fallen behind pace, creates a task in core. This is the piece that makes the agent act without being asked |
| **Approval threshold policy** | Per-action-type limits: auto-approve below a threshold, ask a human in the middle band, refuse above a cap |
| **Business audit log** | Append-only, every actor and every action, with no delete path |
| **The agent loop** | Plan, call a tool, observe, repeat — bounded before it starts by iterations, tool calls, tokens per run and tokens per person per day |
| **Tools** | A shell tool that runs in the sandbox, MCP servers, and HTTP endpoints an operator declared. An agent starts with none of them granted |
| **Sandboxes** | A throwaway container per command by default: no network, no capabilities, read-only root, one workspace per person |
| **Accounts** | Registration closed by default, argon2id, opaque hashed session tokens, every query scoped by `user_id` |
| **Chat channels** | Telegram, Slack and Discord, connected outbound so nothing is exposed. A chat account does nothing until a single-use code links it to an account |

## Requirements

Condensed to what the two services actually implement. The full behaviour is in
[goal-engine.md](goal-engine.md), [core.md](core.md) and [api.md](api.md).

**Goals.** A goal is a target on one declared metric over one period, with a
baseline captured when it is created, a tolerance for being off pace, and limits on
how often falling behind may wake an agent. The operator's own wording is kept
verbatim, because it is what the agent is shown.

**Monitoring.** On a timer, each active goal is evaluated against where it should be
by now rather than against its target. Every evaluation is recorded, including the
ones that decided to do nothing.

**Triggering.** A goal that is behind pace, outside its cooldown, and inside its
trigger budget creates one task in core with a brief that quotes the original
wording and attaches the numbers.

**Running.** Core claims the task and runs an agent loop against it, bounded before the
first iteration by iterations, tool calls, tokens per run, tokens per person per day, a
timeout on each model call and a timeout on each sandbox command. Omitting a limit gets
the restrictive default; zero never means "no limit". Every iteration, every tool call
and every refusal is an append-only row.

**Tools.** What an agent may do is declared in operator-owned YAML with no API write
path, one grant at a time, at a class the operator states: read, write, or spend. A tool
with no grant is denied rather than queried about. A run's grants are snapshotted when it
starts.

**Approvals.** Every action that costs money is decided against operator-owned
policy: auto-approved below a threshold, queued for a human in the middle, denied
above a cap. A missing policy means ask a human. Unanswered requests expire. Core has no
second gate of its own — a spending tool call files with the engine and waits, and a
spending tool with no policy contract is denied.

**Accounts.** People have accounts in core, scoped to their own chats, runs, workspace
and token budget. Goals, money and the brakes belong to whoever runs the instance, not
to a user — because the gate's safety comes from limits an agent cannot reach.

**Channels.** A person can reach the agent from Telegram, Slack or Discord, and every
connection core makes to those platforms is outbound, so nothing has to be exposed to
the internet to use one. A chat account is attached to an account by redeeming a
single-use code minted while signed in and sent over the channel itself — the message is
the proof. Until then a sender reaches exactly one thing: the check that tells them to
link. An inbound message becomes a task on the same path a goal-engine trigger takes, and
spends the budget of the person whose chat it is.

**Audit.** Every decision, dispatch, approval and configuration change is recorded
with its actor, its outcome and the request that caused it.

**Control.** A kill switch that halts every trigger, denies every spend and stops every
agent run; anyone may engage it, only an operator may release it. Unreadable means
engaged.

## Phases

**Phase 1 — the two services.** Built; never yet run against live infrastructure. Core and
the goal engine, both self-hosted, driven by `curl` and by the trigger bridge. Test
personal and business chat, autonomous work in a sandbox, and the approval ladder end to
end.

**Phase 2 — channels.** Built. Telegram, Slack and Discord behind one port, with an
inbound message taking the same path into the agent that a goal-engine trigger takes. A
channel identity is linked to an account before it can do anything. WhatsApp is not among
them: choosing between the official Cloud API and a library that logs in as a person is an
account-risk decision rather than a coding one.

**Phase 3 — clients.** Web is built: one screen holding the approval queue, every goal's
pace, the last tick's decisions, the audit log and a chat, and it holds nothing itself —
no database, no state, and no operator credential in its environment. A desktop shell over
it comes next, then mobile. Mobile last and deliberately: it is the only piece with a
permanent cost in signing and store review.

**Phase 4 — expansion.** The same layer for other products. Evaluate multi-host
deployment and a dedicated cloud sandbox if load demands it. Evaluate native desktop
control only if a business tool requires it.

[roadmap.md](roadmap.md) says honestly where this has and has not got to.

## Risks

| Risk | Mitigation |
|---|---|
| The agent executes something wrong or harmful | A mandatory approval gate for financial actions; a complete audit log; a kill switch; an agent that starts with no tools granted |
| Token and compute costs balloon from autonomous work | Per-run and per-person-per-day token caps with restrictive defaults; a per-goal trigger cooldown and budget; cost reported back as a metric a goal can be written against |
| Prompt injection talks the agent into using a tool it has | Narrow grants, declared one at a time at a class the operator states; spending decided outside core entirely; an unattended write refused rather than queued. Bounded, not prevented — see [SECURITY.md](../SECURITY.md) |
| A container is a shared kernel, not a wall | No network by default, no capabilities, read-only root, one workspace per person; a dedicated sandbox provider on the roadmap; do not co-locate anything else on that box |
| A Linux sandbox is not enough for tools that need Windows | Accept the limitation early; evaluate remote Windows only if it becomes a real blocker |

The first row is the one that shaped the codebase. Almost every design decision in
[goal-engine.md](goal-engine.md) and [core.md](core.md) is an answer to "what if the
agent is wrong about this?" — and [SECURITY.md](../SECURITY.md) states plainly which of
those answers hold and which do not.
