# Core

The part of Wingman that does the work: it takes a brief, runs an agent against it
inside a sandbox, and stops. This document is the model; [api.md](api.md) is the
interface and [architecture.md](architecture.md) is the structure.

Core decides nothing about money. It decides how long a run may keep working, what it
may touch, and when to stop — and it asks the [goal engine](goal-engine.md) about
everything else.

- [A run](#a-run)
- [The loop](#the-loop)
- [The stop ladder](#the-stop-ladder)
- [Bounds](#bounds)
- [Tools](#tools)
- [The tool ladder](#the-tool-ladder)
- [Sandboxes](#sandboxes)
- [The prompt](#the-prompt)
- [Accounts](#accounts)
- [Channels](#channels)
- [What core asks the goal engine](#what-core-asks-the-goal-engine)
- [Who hears about it](#who-hears-about-it)
- [When a worker dies](#when-a-worker-dies)
- [Worked example](#worked-example)

## A run

A task arrives — from a person's chat, from a channel, or from the goal engine's
trigger bridge — and a worker claims it. One run is one attempt at one task.

```
task        7c1f…                brief, source, idempotency key
owner       ana@example.com      whose ledger, whose workspace
provider    anthropic            recorded on the run, not looked up later
model       claude-opus-5
limits      15 iterations · 40 tool calls · 250 000 tokens
            2 000 000 tokens/day · 3m per model call · 90s per tool call
state       iterations · toolCalls · tokensUsed    ← what the ladder reads
stop        one of eleven reasons, written exactly once
```

The limits are stored **on the run row**, not read from configuration when somebody
looks at it later. They are the snapshot the run was actually held to: raise a cap
tomorrow and yesterday's run still reads as the run it was.

`state` is the only thing that changes while the run is alive, and it is the only
thing the stop ladder counts. Nothing in the deciding code counts anything for
itself — the counters are passed in.

## The loop

Three properties, stated because a later change could quietly drop any of them:

1. **The ladder is consulted before every iteration**, not once at the start. A kill
   switch engaged while a run is on its fourth tool call has to stop that run, and
   the only way to notice is to look again.
2. **The tool set is taken once, at the start.** A run is judged against the set it
   was planned with, so a server that gains a capability mid-run does not hand it to
   a run already under way.
3. **Exactly one stop reason is recorded per run**, by the single write that ends it.
   A run with two reasons is a run whose transcript cannot be read.

Three facts come from outside the run and are re-read every iteration. None of them
can fail into a permissive answer:

| Fact | Read from | If it cannot be read |
|---|---|---|
| kill switch | the goal engine, `GET /v1/flags/kill-switch` | **engaged** |
| cancellation | this run's own row | **cancelled** |
| today's token spend | `token_spend`, for this owner | **no answer** — the ladder stops on it |

Then one iteration:

```
domain.Decide(state, limits, ledger, killSwitch, cancelled, providerFailed, finished)
  │  anything but "continue" ends the run here
  │
  ├─ ask the provider, with maxTokens = what is left of both allowances
  ├─ record the model step  (before the tool calls run, so a run killed
  │                          between the two still has the model's reasoning)
  ├─ charge the tokens to the owner's day
  └─ act on the finish reason
```

`maxTokens` is the smaller of what the run has left and what the owner has left today
— zero when the ledger is unreadable. Capping at the provider is exact. The
alternative, estimating what the next call will cost and refusing in advance, is a
guess, and a guess that runs high refuses runs that would have fitted comfortably.

What the loop does with each vendor finish reason:

| The vendor said | The loop does |
|---|---|
| it answered | the turn is finished; the ladder records `completed` on its next pass |
| it refused | **also finished.** A refusal is an answer, and the words are in the transcript |
| the reply was cut short, or paused | one nudge to continue, which costs an iteration and its tokens |
| it wants a tool | run the calls it asked for |
| nothing usable, or a reason this version does not map | `provider_error` |

A model refusal does not get a stop reason of its own. The set is closed, and
inventing a twelfth for "the model said no" would be an API change to describe
something the transcript already says.

A model call that fails is retried at most three times, two seconds apart, and only
for errors the provider adapter recognises as retryable — an unrecognised failure is
not retried, because a request nobody can account for is not a request to repeat. A
retried attempt costs no iteration and no tool call; it bought nothing. What it does
cost is wall clock, which is what the three-attempt bound is for.

Every iteration and every tool call is a `run_steps` row, **including the refused
ones**. A run log that only records what succeeded cannot answer "what did it try?".
There is no update and no delete for those rows.

## The stop ladder

Checked in this order, before every iteration. The order *is* the safety model:

| # | Condition | Stop reason |
|---|---|---|
| 1 | the model answered without asking for another tool call | `completed` |
| 2 | a human cancelled the run | `cancelled` |
| 3 | the last model call failed in a way the loop cannot retry | `provider_error` |
| 4 | the kill switch is engaged, or unreadable | `halted` |
| 5 | the spend ledger gave no answer, or its counter went backwards | `budget_unreadable` |
| 6 | this run has spent its token allowance | `run_budget_exhausted` |
| 7 | the owner has spent today's allowance | `user_budget_exhausted` |
| 8 | the loop has gone round as many times as it may | `iteration_cap` |
| 9 | the run has made as many tool calls as it may | `tool_call_cap` |
| 10 | anything left | **continue** |

Rows 4 to 9 are the guards on *continuing*. They sit below rows 1 to 3 on purpose:
they answer "may this run do more work?", and that is not a question worth asking
about a run that is already over.

Which is why **`completed` sits above `halted`** — the one place this ladder departs
from the obvious ordering, and the one worth arguing for. A run whose model has
already answered will spend nothing further, so filing it as `halted` would record a
finished run, including whatever its tool calls already did, under a word that means
"nothing happened". Concealing side effects that did happen is the worse failure of
the two. The kill switch loses nothing by it: it is checked once before the first
iteration, where nothing has run yet and `halted` is the plain truth, and from then
on it stops every iteration of every run that is not already finished.

Rows 5 and 6 mirror the goal engine's spending gate: caps deny rather than escalate,
and no budget question is decided against a ledger that cannot be read. A run stopped
as `budget_unreadable` is waiting on a repair, not on tomorrow — which is why it is
not filed as an exhausted budget.

Rows 8 and 9 are separate because they describe different runs. One talked too long;
the other acted too much. An operator tuning limits needs to know which.

Two reasons the ladder never produces:

| | Decided by | Why it is not a ladder branch |
|---|---|---|
| `tool_denied` | `ClassifyTool`, per call | It is about what a run may *do*, not what it may consume |
| `abandoned` | the recovery sweep | Nothing inside a run can conclude this about itself |

That makes **eleven**, and the set is closed: `run_stop_reason` is a Postgres enum, so
a twelfth reason cannot be stored, and `GET /v1/reference` returns the eleven in the
order above from the same list the ladder is written against.

## Bounds

Every field is a ceiling, and a zero in any of them **stops the run** rather than
releasing it. There is exactly one place that turns an omitted value into a working
number, and a run that reaches the ladder without passing through it halts
immediately — which is the failure mode to want if the wiring is ever wrong.

| | Default | Floor | Ceiling | Bounds |
|---|---|---|---|---|
| `RUN_MAX_ITERATIONS` | 15 | 1 | 200 | how many times the model may be asked |
| `RUN_MAX_TOOL_CALLS` | 40 | 1 | 500 | side effects, not thought |
| `RUN_MAX_TOKENS` | 250 000 | 1 000 | 10 000 000 | one run's allowance |
| `USER_DAILY_TOKEN_CAP` | 2 000 000 | 1 000 | — | every run one person starts today |
| `RUN_STEP_TIMEOUT` | 3m | 5s | 15m | one model call |
| `SANDBOX_TIMEOUT` | 90s | 1s | 30m | one tool execution |

The defaults are restrictive on purpose. An operator who raises one is making a
decision; an operator who leaves a field blank is not.

`USER_DAILY_TOKEN_CAP` is the only limit that can be switched off, and it has to be
written out as `-1`. Any other negative value is rejected rather than read as
unlimited, because reading a typo as "no limit" is the most expensive possible
interpretation of one.

Two bounds are not configurable:

**A single turn's tool calls.** The ladder checks the tool-call cap *between*
iterations and cannot check it inside one: a single turn may ask for fifty calls, and
running them all before the next consultation would put the cap thirty calls behind
the run. So the count is checked before each call within the turn as well, and
reaching it there ends the run as `tool_call_cap`.

**Tool output: 64 KiB per call**, keeping the head and the tail with a count of what
was dropped between them. Tool output goes straight into the next model call, so an
unbounded one is a run spending its whole allowance on a single `cat` of a log file —
and then stopping with `run_budget_exhausted` having done nothing. The head says what
the command was doing; the tail holds the error it died of.

## Tools

A tool needs **two** things to be callable, and neither is allowed to be the whole
answer:

- **a grant** — the operator's declaration of its class and whether it is on, in
  `config/tools.yaml`, which no API writes;
- **a runner** — something that knows how to execute it.

| Source | What it is | Declared in |
|---|---|---|
| `builtin` | one `shell` tool, which runs its script in the sandbox | — |
| `mcp` | tools an MCP server offers, named `mcp__<server>__<tool>` | `config/mcp.yaml` |
| `http` | endpoints of an HTTP API the operator wrote down | `config/http-tools.yaml` |

The split is the point. What is *technically reachable* comes from servers that may
add capabilities overnight. What an autonomous agent *may do* stays in a file an
operator owns — the same rule the goal engine applies to metric definitions, for the
same reason: a list of what an agent may do is not a list an agent may edit.

**All three files ship listing nothing**, so a fresh instance can read and think and
cannot touch anything. Granting the first tool is a deliberate step, and
[deployment.md](deployment.md#8-grant-core-its-first-tool) is where it is written
down.

What the registry does when it assembles a run's offering:

- A name with no grant, or a grant switched off, **is not offered** — whatever a
  server says it can do. The per-call ladder would deny it anyway; not showing it
  means the model never plans around a capability it cannot use.
- A source that cannot be listed **does not stop the run**. The run proceeds with
  fewer tools, the missing sources are named in its prompt, and the operator gets a
  log line. The failure mode of proceeding is an agent that cannot do something and
  says so; the failure mode of refusing is one unreachable MCP server halting every
  run on the instance.
- A granted name claimed by **two** sources is **withdrawn from both**. The grant says
  what class the tool has and therefore whether it needs a human; it does not say
  whose implementation runs, and picking the first would let a source added later
  quietly take over a name the operator approved for another one.
- The list is **sorted**, so two identical runs get identical prompts and a vendor's
  prefix cache is not defeated by map ordering.
- A name must match `^[a-zA-Z0-9_-]{1,64}$` — the intersection of what both vendors
  accept — and this is checked when the grants are loaded. One badly named tool from
  one MCP server would otherwise be rejected with the whole request, breaking every
  call the run makes rather than the one that uses it.

## The tool ladder

Every individual call is decided again, against the grants, in this order:

| # | Condition | Verdict |
|---|---|---|
| 1 | no name, or no grant for this name | **denied** |
| 2 | the grant is switched off | **denied** |
| 3 | class is `read` | allowed |
| 4 | class is `write`, and the run is attended | allowed |
| 5 | class is `write`, and the run is unattended | approval required |
| 6 | class is `spend`, with a contract declared | approval required, via the goal engine |
| 7 | class is `spend`, with no contract | **denied** |
| 8 | no usable class | **denied** |

Row 1 is the load-bearing one. An unknown tool is denied, not passed through — and
because the offering already excluded everything ungranted, a name that fails here is
one the model invented or one withdrawn since the run started. Both **end the run**.
The model was shown exactly what it could use, so a call outside that set is not a
mistake to negotiate over: an agent told "no, try again" learns to keep guessing.

Row 5 turns on whether somebody is reading the reply as it happens.

| Task source | Attended |
|---|---|
| `user` — somebody typed it | yes |
| `channel` — somebody typed it in Telegram, Slack or Discord | yes |
| `goal_engine` — a metric moved at 04:00 | **no** |
| anything unset | **no** — work whose origin is unclear is the careful reading |

A person in a chat can undo a wrong message in seconds; a trigger firing overnight
cannot. Same tool, same class, different exposure.

Row 6 sends money decisions somewhere else on purpose. The goal engine already has a
deny-biased ladder with caps that refuse rather than escalate, a missing policy
meaning "ask a human", and an audit row per outcome. A second gate in core would be a
weaker copy of it, and the weaker of two gates is the one that decides.

What happens after the verdict:

| Verdict | The run |
|---|---|
| allowed | executes the call, bounded by `SANDBOX_TIMEOUT` |
| unattended write | is told the call was refused **for this run**, and carries on |
| spend, `auto_approved` | executes the call |
| spend, `pending` | is told a human was asked, and carries on without it |
| spend, `denied` | stops: `tool_denied` |
| spend, gate unreachable | stops: `tool_denied` |
| spend, an outcome this version does not recognise | stops: `tool_denied` |
| denied | stops: `tool_denied` |

**An unattended write is refused, not queued.** There is no gate denominated in
anything a write can be measured in: the goal engine's policies are amounts in a
currency, and filing a write there as an amount of nothing would be refused for
having no amount, in an audit row that blames the number instead of the situation. So
the model is told, and the run stays able to finish and report what it did manage. It
is also told this at the start, because a model that keeps retrying a refused write
burns the whole run discovering the same answer.

**Pending is not a wait.** Core does not hold a run open against a human's attention:
a parked run holds a worker, a sandbox and a place in somebody's daily budget for as
long as nobody looks at the queue. Telling the model instead leaves a transcript
saying what it wanted to do and why it could not — which is what the person
approving the payment needs to read anyway.

**No goal engine means nothing may spend.** An instance configured without one has no
gate, and a spending call is refused rather than waved through. That is the correct
reading of "the thing that decides is not there".

Two smaller rules in the same place. The amount a spending call would move is read
out of the model's own arguments and handed to the gate, whose caps are what decide —
but a missing or unparseable amount must never arrive there as a zero, because zero
is below every threshold an operator would write; the model is told to fix its
arguments instead. And a tool that *ran* and failed is not a failure of the run: the
output goes back with an error flag, because working around a command that did not
work is what the model is there for, and ending the run would make one missing file
lose everything already established.

## Sandboxes

`SANDBOX_BACKEND` picks one. Both share three rules that are not backend details: a
command runs inside the run's workspace and nowhere else; a command never inherits
this process's environment, because that environment holds the provider keys, the
database password and the goal engine's key; and a command that outlives its deadline
is killed and reported as output the model can read, while a command somebody
cancelled ends the step instead.

`docker` is the isolation. Every flag is load-bearing:

| Flag | Why |
|---|---|
| `--network none` | The default is a bridge with working internet access. A sandbox that can reach the network is a different threat model, and one an operator turns on deliberately |
| `--memory`, `--memory-swap` **equal** | Without the second, the limit is a limit on speed rather than on memory |
| `--pids-limit 256` | A fork bomb inside the sandbox does not become one on the host |
| `--cap-drop ALL` + `--security-opt no-new-privileges` | A tool call needs no capabilities and no way to acquire any: dropping them still leaves a setuid binary able to escalate |
| `--read-only` + `--tmpfs /tmp` (64 MiB) | A tool writing outside the workspace is writing somewhere that vanishes, so failing loudly beats succeeding invisibly |
| `--volume <workspace>:/workspace`, `--workdir`, `HOME=/workspace` | One fixed path, so a path in one run's transcript means the same thing in another's |
| `--user <uid>:<gid>` | Files the agent creates belong to the service, not to root. Without it, one run leaves a workspace this service can no longer write to |
| `--entrypoint sh` | An image is a filesystem here, not an application. Honouring an `ENTRYPOINT` would make one image swap change what every script means |
| `--rm`, a random `--name`, a fixed label | `--rm` only fires when the container's own process ends, so a killed client's container is removed by name; the label is how an operator finds strays after an outage |

The name is unguessable rather than sequential, because the name is what the cleanup
acts on: a predictable one would let anything else with a docker socket delete a
container by guessing which run was next.

Workspaces are **one directory per person**, `0700`, created on first use and reused
afterwards — a run that starts from an empty directory cannot build on what the last
one left. Nothing is shared: two people sharing a workspace is one of them reading the
other's data through a tool call that looks entirely legitimate in the audit log.
Every path is checked to be inside the configured root before a process starts, with
symlinks resolved first, since a workspace that is a link to `/` would pass a purely
lexical check and then be mounted into a container as though it were one person's
files.

`local` **is not isolation and does not pretend to be.** It runs the script on this
host as the service user, with the environment stripped and the workspace and timeout
rules still applied. It exists so a laptop without a docker daemon can drive a run.
Do not run it in front of other people; see
[deployment.md](deployment.md#running-without-docker).

## The prompt

The system prompt is **byte-identical for every turn of one run**, which is what lets
both vendors serve the prefix from cache instead of billing it again each time. So it
carries no counters — no "you have used 4 of 10 turns". The bounds are the ladder's
business, and the ladder stops the run without needing the model's cooperation.

It does state the numbers. A model told it has fifteen turns spends them differently
from one told to be brief, and the bounds are enforced whether or not it knows them —
so hiding them only removes the chance of planning.

It says whether anybody is waiting, because that is the difference between a run that
can ask a question and a run whose question nobody will ever see. An unattended run
is told to name the decision, recommend something and stop rather than ask.

It names the tool sources that were unreachable when the run started, so a task
written against a missing capability is reported as blocked instead of attempted three
times — the labels only, never the underlying error, which can quote the URL a source
was configured with.

The brief itself arrives as a **user** message, not as part of the system prompt.
It came from a chat, a channel or a trigger, and text that came from outside must not
sit in the part of the prompt that grants permissions. Dispatch metadata is rendered
under a line saying it is facts about the request rather than instructions, because a
value in a map somebody else filled in is still a value even when it reads like an
order.

## Accounts

Accounts live in core and stop at core's edge. Goals, money and the brakes belong to
whoever runs the instance — see [architecture.md](architecture.md#why-two-services)
for why that boundary is where it is.

**Passwords** are argon2id at the current OWASP figures: 64 MiB of memory, three
passes, four lanes, stored PHC-encoded so the parameters travel with the hash and
raising them next year still verifies every password hashed this year. They are
constants rather than configuration: an operator tuning password hashing down to save
memory on a small VPS is making a security decision while thinking about a
performance one. The only rule on a password is length — 12 to 256 characters.
Composition rules push people toward predictable substitutions and a sticky note, and
they are not what stands between an attacker and a 64 MiB hash.

Sign-in pays the argon2id cost **even for an address with no account**, by verifying
against a hash generated at startup that no password matches. The gap between a
microsecond refusal and a hundred-millisecond one is an account-enumeration oracle
measurable over the internet: ask about a thousand addresses, and the slow answers are
the real ones.

**Sessions** are 256 random bits with a `wgm_` prefix, and only their SHA-256 is
stored, so a database dump is a list of expired opportunities rather than a set of
live logins. SHA-256 and not argon2id, deliberately: there is nothing to guess in 256
uniform random bits, and the hash is computed on every authenticated request, where
64 MiB of argon2 per call would be a denial of service with a security rationale
attached. The prefix exists so a token pasted into a bug report is recognisable as a
live credential and can be revoked.

Everything else follows from those two:

- Registration is **closed by default**. A self-hosted box found on the internet with
  open sign-up is a box running strangers' code in your sandbox on your API key. The
  first account is made by `core createuser`, which reads the password from stdin.
- Changing a password ends **every** session, including the one that changed it.
- Deactivating an account ends its sessions and keeps everything it did. The audit
  trail is the point.
- Every query over user content carries `user_id` in the `WHERE` clause rather than
  filtering after the read.
- Somebody else's run id answers `404`, not `403`. An id that can be confirmed can be
  enumerated.
- Unattended work — a task from the goal engine, with nobody behind it — is filed
  against the account named in `CORE_UNATTENDED_OWNER`, resolved to an id once at
  boot. A run with no owner is a run whose tokens appear in no ledger and whose
  transcript nobody can find, so the variable is required as soon as a bot key is set
  and has no default.

## Channels

Core can be reached on Telegram, Slack and Discord as well as over HTTP. All three are
off unless an operator sets a token; an instance with none configured has no inbound
chat path at all, which is the default.

Every connection is **outbound** — Telegram long polling, Slack's Socket Mode socket,
Discord's gateway. So none of this needs a public address, a certificate, a forwarded
port, or a route that verifies somebody else's request signature, and a box behind a
home router works with nothing opened. There is no channel webhook endpoint in
[api.md](api.md) because there is nothing for one to receive.

A message that is accepted becomes a task on **the same path** a goal-engine trigger
uses. There is one way into the agent loop, and the ladder below is what stands in
front of it.

### Linking

A chat account can do nothing until a person attaches it to their Wingman account.
Signed in, they mint a code — `WGM-` and ten characters from a thirty-character
alphabet, a little under 49 bits — and send that code to the bot on its own.

- **One live code per account.** Minting invalidates the outstanding one, so a code
  left in a scrollback is already dead.
- **Stored as a SHA-256 hash**, like a session token. The plaintext is returned once
  and exists nowhere afterwards.
- **Single use, and it expires** after `CHANNEL_LINK_CODE_TTL` — 15 minutes by default,
  floored at 1 minute and capped at 1 hour. Whoever sends it attaches *their* chat
  account to whoever minted it, so it is a bearer credential and the window is short on
  purpose.
- **Unknown, expired and already spent are one answer.** A sender who can tell them
  apart has been handed the difference.
- The whole message has to be the code. Scanning prose for something code-shaped would
  consume a live credential on a false positive — and one that may not belong to the
  sender.
- The alphabet excludes `0/O`, `1/I/L` and `U`. A code that cannot be transcribed gets
  pasted wrong, and every wrong paste is another minute the real one stays live.

Disconnecting revokes the row rather than deleting it, so the runs it started keep their
provenance. A revoked chat account is a stranger again, and it can only be picked up
by **the same Wingman account** that released it — a fresh code redeemed from anywhere
else is told an operator has to clear it, without being told whose it was.

### The inbound ladder

Seven rungs, in this order, producing one of eight verdicts. The order is the safety
model and reordering it is a behaviour change, so there is one test per rung.

| # | Condition | Verdict | Reply |
|---|---|---|---|
| 1 | the platform says the author is an application | `ignore` | — |
| 2 | no text to act on — a sticker, an uncaptioned photo, a join | `ignore` | — |
| 3 | a shared room, and `CHANNEL_ALLOW_GROUPS` is off | `group_refused` | direct messages only |
| 4 | the sender is not linked, and the message is a code | `link_attempt` | connected, or the code is refused |
| 4 | the sender is not linked, and it is not | `link_required` | how to link, and nothing else |
| 5 | this sender's last accepted message is inside `CHANNEL_MIN_INTERVAL` | `throttled` | — |
| 6 | longer than a brief may carry | `too_long` | how long is too long |
| 7 | the kill switch is engaged, or could not be read | `halted` | halted, an operator must release it |
| — | otherwise | `accept` | — |

Three things about that order are deliberate:

- **Loop prevention is rung 1**, above everything. Two applications answering each other
  costs real money per exchange, and every rung below this one replies.
- **The room is settled before the sender**, because whether a shared room may be used at
  all is a fact about the room. Telling a stranger in a public channel how to link
  would invite an account credential into a place other people can read.
- **The kill switch is last**, because it is the only fact here that costs a network
  call. A flood of messages must not become a flood of requests to the goal engine — and
  reaching rung 7 is what makes an unreadable switch mean engaged here too.

One consequence follows from that and is intended: a person pasting a link code is
served while the instance is halted. Linking spends nothing and starts no run, and
answering somebody mid-connection with silence gives them nothing to act on.

Two verdicts say nothing back. Every other one answers, including every refusal — a
self-hosted assistant that goes quiet is indistinguishable from a broken one, and the
person on the other end has no logs to read. No reply ever names an account, a user id,
a database or which of several reasons applied.

`accept` also says nothing, because the answer is the run's: it is delivered into the
same conversation when the run finishes. An acknowledgement would double the traffic to
say what the next message says anyway.

### What a channel run costs, and whose

The run is filed against the person who linked the chat, spends their daily token
allowance, and is subject to every bound in [Bounds](#bounds) unchanged. There is no
separate, looser path for chat.

Which is the whole reason `CHANNEL_ALLOW_GROUPS` is off by default: in a shared room the
linked person's budget is spendable by anybody who can type there, and the run is filed
against them. Turning it on logs a warning at startup. Even on, core only acts on a
group message that mentions the bot.

`CHANNEL_MIN_INTERVAL` has a hard floor of one second for the same reason every other
limit does — zero would read as "no throttle", and this is the only decision in core a
stranger can drive. Anybody who can find the bot can send it messages, and each accepted
message is a run.

### Sending

Long answers are split into as many messages as the platform takes, sequentially,
stopping at the first failure: a dropped connection fails on every chunk, and eight
attempts against one outage produce eight log lines about it. A partial delivery is the
honest outcome, and the whole answer is in the chat either way.

Two platform-specific rules, both about an answer becoming something nobody asked for:

- **On Discord nothing in an answer may mention anybody.** A model summarising an outage
  writes `@everyone` without meaning to notify a server. The words arrive exactly as
  written; Discord is told to resolve no mentions at all.
- **On Slack the text is escaped.** `&`, `<` and `>` are markup there, so unescaped model
  output could render as something other than what was written — including a link a
  person did not ask for.

On Telegram there is no parse mode. A model's answer routinely contains asterisks,
underscores and backticks that Telegram's Markdown parser rejects as unbalanced, and a
`400` on a correct answer is worse than plain text.

Adapters ask their platform who they are before reading anything, over the network. It
is the one call that fails loudly on a bad token: long polling retries a rejected
`getUpdates` for ever and a gateway that will not authenticate reconnects quietly, so
without it a wrong token would be indistinguishable from a quiet platform. Building the
adapters, by contrast, touches nothing — a platform having an outage while core starts
must not stop core starting, because the other platforms and every HTTP route still
work.

Discord is asked for **three intents and no more**: guild messages, direct messages,
message content. An intent not asked for is data that never reaches this process.

## What core asks the goal engine

Three calls, all outbound, all with core's **bot** key on the engine — never an
operator key, or the service running agent-authored output could resolve its own
approval requests and the gate would be decorative.

| When | Call | If it fails |
|---|---|---|
| before every iteration | `GET /v1/flags/kill-switch` | the run **halts** |
| before accepting a channel message | `GET /v1/flags/kill-switch` | the message is **refused**, and said so |
| before a spending tool call | `POST /v1/approvals` | the call is refused and the run stops |
| after a run finishes | `POST /v1/metrics/ops.tokens_spent/samples` | logged, and dropped |

The kill switch is read fail-closed twice over. An unreadable flag is engaged, and a
`200` carrying no flag is **not** a released switch — a proxy, a login page or a
different service on that address can all answer `200` with JSON that decodes into an
empty struct, and every one of those would otherwise read as "nothing is halted".

An instance with no goal engine configured answers "not engaged", and that is the one
place this direction is permissive — because the question was never asked. An empty
base URL is an operator choosing to run core standalone; a base URL that is set and
unanswerable is a fault, and only the second fails closed. There is deliberately no
equivalent for the spending gate: a standalone instance has no gate, and no gate means
nothing may spend.

Cost reporting is what makes the assistant's own spending a metric a goal can be
written against — state a target on `ops.tokens_spent` and the engine notices when the
agent starts costing more than it saves. It is read back from the ledger rather than
taken from the run's own count, because the ledger is what was actually charged. A run
that spent nothing pushes no sample: a zero would be a real data point of "no spend"
in a series an operator writes a goal against. The note carries the run id and the stop
reason and nothing else — never a brief, a model's words or anything a tool returned.

## Who hears about it

The other direction, and the only thing the goal engine asks of core besides a task:
`POST /v1/notifications`. The engine has decided something a person should hear about and
has already recorded it; core owns the chat platforms, so core decides who to tell and
where. See [goal-engine.md](goal-engine.md#telling-a-human) for when one is emitted and
what it may say.

**The recipient is the account `CORE_UNATTENDED_OWNER` names** — the same account
unattended work is filed against, resolved to an id at boot. Every chat identity that
account has linked and not revoked gets the message. There is deliberately **no recipient
field in the request**: one would let whoever holds a bot key send a message from this
instance to any chat account linked to it, and there is nothing to configure here because
that variable is already required wherever `CORE_BOT_KEYS` is set — which is exactly the
deployment where notifications matter.

Three consequences of choosing the owner rather than a target:

- A revoked identity is skipped. Disconnecting a chat account is somebody asking not to be
  messaged there again, and this is where that is honoured — the directory returns revoked
  rows because a person needs to see the ones they disconnected.
- An owner with nothing linked is a **warning and a `200`**, not an error. It is how every
  instance starts, and the answer says so plainly so an operator who turned notifications
  on and heard nothing learns the linking step is outstanding: sign in as the owner, mint a
  link code, redeem it from the chat account that should get the messages.
- One platform failing does not cost the others their message. A bot blocked on Telegram is
  that person's own choice; the run continues down the list and every failure is a warning
  carrying the platform, the kind and the subject — never the text and never the
  conversation.

The notifier is the least powerful service in the package on purpose. It reads one
account's chat identities and writes to none of them: it cannot revoke a link, start a run,
read a chat or see anybody's rows but the owner's, which is why it takes a directory port
rather than the one that also carries `Revoke`. And it is refused to a signed-in person,
operator session included — the same rule as dispatch, so that nobody can make this
instance message the operator's own phone.

Sending needs one platform-specific step. Telegram and Slack take the person's own id
where a conversation id goes — a Telegram private chat has the same id as the person in it,
and `chat.postMessage` opens the direct conversation on Slack's side. **Discord does not**:
a user snowflake is not a channel, so the adapter calls `UserChannelCreate` to open the DM
and sends to the channel that comes back. It is idempotent — Discord hands back the
existing DM when there is one — so there is nothing cached and nothing to clean up.

Core adds no wording. The headline arrives as the engine wrote it, the link goes on a line
of its own, and a link that is not `http` or `https` is refused rather than sent: it is the
one part of a notification somebody is invited to act on, and it arrives carrying this
instance's credibility.

## When a worker dies

A crash, a `kill`, a deploy mid-run: the run row stays in flight and nothing inside it
can say so. Three sweeps run alongside the workers, once at startup and then every five
minutes. The first closes work left behind — the run is failed as `abandoned` and its
task is requeued, runs first and tasks second, because the reverse order can leave one
task with two live runs and a transcript that cannot be read as one story. The second
deletes expired sessions and the third drops spent and expired link codes; both of those
are hygiene, because `Resolve` already refuses an expired session and a spent code is
already refused when it is redeemed. Link codes are swept even on an instance with no
channel connected — they were mintable before the token was taken out of the
environment.

Startup first, then the ticker, because the most likely reason a run is abandoned is the
crash this process just restarted from, and a task left claimed by a worker that no
longer exists is a task nobody would otherwise pick up. A failure in any one of the three
is logged and does not stop the other two.

How long a run may be in flight before that happens is **derived from the operator's
own limits** rather than configured separately, because the only correct answer is
"longer than a legitimate run":

```
2 × RUN_MAX_ITERATIONS × (RUN_STEP_TIMEOUT + SANDBOX_TIMEOUT), never under 5 minutes
```

An operator who allows two hundred iterations of fifteen minutes has runs that take
days, and a fixed timeout would requeue them while they were still working.

## Worked example

An unattended run: the goal engine noticed MRR was 12% behind pace and dispatched a
brief. Limits are the defaults; the operator has granted three tools.

| # | Ladder says | The turn | Recorded |
|---|---|---|---|
| 1 | continue — iteration 1 of 15, 250 000 left | asks for `read_report` | model step, then a tool step |
| | | `read` → allowed, runs in the sandbox | |
| 2 | continue — iteration 2 of 15 | asks for `post_update` | model step, then a tool step with the refusal |
| | | `write`, **unattended** → refused for this run; the model is told why | |
| 3 | continue — iteration 3 of 15 | asks for `pay_invoice`, 250 000 IDR | model step, then a tool step |
| | | `spend` → filed with the goal engine, which answers `pending` | |
| | | the model is told a human was asked; the run carries on | |
| 4 | continue — iteration 4 of 15 | answers with a summary and no tool call | model step |
| 5 | **`completed`** | — | the run row, once |

Afterwards: the task is marked succeeded, the run's token use is pushed to the engine
as `ops.tokens_spent`, and because nobody was in a chat, the transcript is the whole
record.

Row 2 is the one worth reading twice. Nobody was watching, so the write did not
happen — and the run did not fail, did not stall, and did not queue a message for
somebody to approve at midnight. It said so, kept working, and finished with a report
that names what it could not do.

Row 3 is the same shape one layer up: core did not decide the payment, did not wait
for a human, and did not lose the reason. The gate decided; the transcript remembers.
