# Security

Wingman lets an autonomous agent act on a business and spend money. The parts of it
worth attacking are obvious, so here is what the project claims, what it does not,
and how to report a hole in either.

- [Reporting a vulnerability](#reporting-a-vulnerability)
- [Scope](#scope)
- [Threat model](#threat-model)
- [What the goal engine guarantees](#what-the-goal-engine-guarantees)
- [What core guarantees](#what-core-guarantees)
- [What the console guarantees](#what-the-console-guarantees)
- [What it does not](#what-it-does-not)
- [Hardening a deployment](#hardening-a-deployment)
- [Known limitations](#known-limitations)

## Reporting a vulnerability

**Do not open a public issue.**

Use GitHub's private reporting: **Security → Report a vulnerability** on
[the repository](https://github.com/ribdsp/wingman/security/advisories/new). If that
is unavailable to you, open an issue titled "security contact request" with no
detail in it, and a private channel will be arranged.

Include, as much as you have:

- What you did and what happened.
- Which component — `goal-engine`, `core`, the console in `web/`, the compose stack, or
  the docs.
- The commit or tag.
- What an attacker gets out of it.
- A reproduction, if you have one. A failing test in either service's
  `internal/domain` is ideal.

What to expect: an acknowledgement within a few days, an assessment, and a fix or a
documented "this is intended, here is why". This is a small project without an
on-call rotation — an honest timeline is days for a reply and longer for a fix,
depending on severity.

If you would like credit in the advisory, say so. If you would rather not be named,
say that instead.

Please do not test against anyone else's deployment. Self-host it; the whole thing
comes up with one compose command.

## Scope

**In scope**

- `goal-engine/` — the API, authentication, the two privilege levels, the evaluation
  and approval ladders, the kill switch, the audit log, SQL construction, the metric
  samplers.
- `core/` — authentication and sessions, per-user scoping of every query, the
  stop-reason and tool ladders, the sandbox boundary, MCP and HTTP tool execution,
  the spending path to the engine's gate, secret handling in tool config.
- `web/` — the console. Cookie sealing, the list of routes it will forward and the
  authority each is sent with, how a pasted operator key is verified and how long it is
  held, the Content-Security-Policy, and anything by which a credential could reach a
  browser or a response body.
- `deploy/docker-compose.yml`, all three `Dockerfile`s — anything that exposes a port,
  leaks a secret into an image layer, or runs something as root.
- Documentation that tells an operator to do something unsafe. A wrong instruction
  in [deployment.md](docs/deployment.md) is a real vulnerability, and it is the
  cheapest kind to fix.

**Out of scope**

- A deployment you configured insecurely (`0.0.0.0` bindings, a shared operator key,
  a read-write DSN for a `sql` metric, `SANDBOX_BACKEND=local` on a host that matters,
  the console served to anything but loopback without TLS in front of it).
  Worth an issue if the docs led you there.
- A tool you granted in `config/tools.yaml` doing what you granted it. Classifying a
  write tool as `read` is a configuration mistake, and the file says so in its own
  comments; a *wrong* classification applied by the code is in scope.
- An MCP server you chose to run. `npx -y some-package` executes whatever that name
  resolves to today, which is a supply-chain decision you are making.
- Missing rate limits on `/healthz`.
- A browser that is already compromised — an extension with access to the console's
  origin, or a machine somebody else is typing on while authority is held. The console
  keeps its credentials out of the page; it cannot keep them from a browser that is
  no longer yours.
- Anything requiring an operator key you were legitimately given. An operator can
  raise every cap by editing a file; that is the design.
- Denial of service by holding a valid credential and hammering the API.
- Prompt injection producing a *permitted* action. That the agent can be talked into
  using a tool you granted it is the reason the grants are narrow; that it can be
  talked past a grant is a vulnerability.

## Threat model

The adversary this system is built against is **not** primarily a remote attacker.
It is a **capable agent behaving badly** — looping, misreading a metric, being
prompt-injected by a webpage it read, or simply wrong. Remote attackers matter too,
and are the more conventional half.

| Adversary | Has | Should not be able to |
|---|---|---|
| **A misbehaving or prompt-injected agent** | A run inside core, and a bot key against the engine | Call a tool nobody granted, treat a `write` tool as attended when nobody is watching, spend without a policy contract, exceed its iteration, tool-call or token caps, move a goal's target, define a metric, resolve its own approval, release the kill switch, raise a cap, wake itself faster than the cooldown, make the box message a chat account of its choosing, alter the audit log |
| **A stolen bot key** | The same | The same, plus: report metric values that are indistinguishable from a legitimate feed's — see [limitations](#known-limitations) |
| **A stolen operator key** | Everything a human operator has | Nothing is claimed. This is game over, and the audit log's only job is to say what was done with it |
| **A signed-in core user** | An account and a session token | See or cancel another user's runs, read another user's chats, reach another user's workspace, change a tool grant or a run limit, create an account, resolve a spend, dispatch a system task, or make core send a notification to the unattended owner's chats |
| **Code running inside the sandbox, `SANDBOX_BACKEND=docker`** | A workspace, a shell, and whatever network the operator allowed | Reach a provider API key, either database, the engine's credential, the docker socket, or another user's workspace |
| **Code running inside the sandbox, `SANDBOX_BACKEND=local`** | The service's own user on the host | Nothing is claimed beyond a minimal environment and a deadline. This backend is not isolation — see [limitations](#known-limitations) |
| **Unauthenticated network access** | Reachability | Anything at all except `/healthz` and `/readyz`, and — if you turned it on — registration |
| **A copy of the console's cookies** | Two sealed envelopes, off a laptop or out of a proxy log | Be used against anything. Each is AES-256-GCM with the purpose bound in, so without `WEB_COOKIE_SECRET` neither opens, and one cannot be replayed as the other |
| **Script running on the console's origin** | Whatever a page can do | Read either cookie or lift a credential out of the page: both cookies are httpOnly, and no credential is ever serialised into the HTML, a response body or a bundle. It *can* act as the signed-in person on the routes the console forwards, which is why `script-src` carries a per-request nonce, never `'unsafe-inline'`, and has a test |
| **A compromised console** | The engine bot key, plus the sealed cookies of whoever is signed into it | Change any decision in either service, or reach a person who is not signed into it. What it can do is what those people could do, plus engage the kill switch, which anyone may |
| **A compromised core** | The task route's credentials, and whatever core's sandbox reaches | Change any decision in the goal engine. Core is a client of that API, not a peer: it asks, it does not decide |
| **A read-only database credential for a metric** | One query against your product database | Write anything, or be used for a second statement |

The first row is the one that shaped the codebase. Almost every design decision in
[goal-engine.md](docs/goal-engine.md) is an answer to "what if the agent is wrong
about this?".

## What the goal engine guarantees

Each of these is enforced in code, not documented as a convention.

**Privilege is fixed at startup.** A credential is an operator key or a bot key
according to which environment variable listed it — `GOAL_ENGINE_API_KEYS` or
`GOAL_ENGINE_BOT_KEYS`. No header, body field, query parameter or audit field can
raise a role. There is no privilege-escalation path because there is no privilege
grant path.

**Operator-only operations are checked twice.** `middleware.RequireOperator` guards
the route; the service checks again through `Actor.requireOperator`. A route guard
protects a route. The service guard protects the invariant when a second route, a
CLI, or a scheduled job reaches the same operation.

**An agent cannot move its own goalposts.** Editing a goal's target, period or
status is operator-only. The alternative is an agent that resolves being behind by
lowering the bar.

**An agent cannot define a metric.** Definitions carry SQL text and the names of
environment variables holding database passwords. They exist only in
operator-owned YAML, read once at startup, with no API write path. The public
metric view publishes the key, description, unit and source — never a query, URL,
datasource or credential name.

**An agent cannot set its own limits.** Spending policies live in the same
operator-owned YAML. A goal may only reference a declared metric.

**A missing policy asks a human.** `config/policies.yaml` ships empty. An action
type with no policy is `pending`, never auto-approved. A limit nobody wrote down is
a question, not permission.

**Caps refuse rather than escalate.** Past `hardCap` or `dailyCap` the request is
denied outright, not queued for approval. An operator asked to rubber-stamp a cap
breach will eventually rubber-stamp it.

**A broken ledger denies.** If today's recorded spend cannot be read, or reads
negative, every request is denied. No deciding against a ledger that is not
answering.

**The kill switch is one-way for agents.** Any credential may engage it — an agent
that notices it is doing damage must be able to stop the fleet. Only an operator may
release it. If the flag row cannot be read at all, the engine treats the switch as
engaged: unreadable means halt.

**The audit log is append-only.** Not "admin-only" — there is no service method that
updates or deletes a row. Every entry carries the actor, the credential's name, the
action, the subject, the outcome and the request ID.

**A notification cannot be aimed, and carries no figures.** When `NOTIFY_ENABLED` is on
— it is off by default — two moments post one line to core: a trigger was dispatched, and
a spend came back `pending`. The request carries a kind, a subject id, one sentence and a
console link, and **no recipient**: a bot key cannot make your instance send a message to
an address of its choosing, because there is no field in which to name one. Nor can it
send figures. No amount, no currency, no observed value, no pace ratio — a test asserts
the headline contains no digit at all, because a chat message is retained on somebody
else's servers and because a message complete enough to decide from is one people decide
from at a glance.

**A notification can never affect a decision.** It is emitted *after* the decision it
describes is already durable, its failure is logged and dropped rather than returned,
nothing is retried and nothing is queued. A lost message costs a prompt, never a
decision: the approval is still in the queue, the console still shows it, and
`APPROVAL_TTL` still expires it.

**SQL is parameterised.** No ORM, no string-built queries, one repository type per
table. Metric queries from YAML are checked to be a single read *and* executed in a
`ReadOnly` transaction with a timeout — the string check is a first pass, the
transaction is the guarantee, and a read-only database role is what you should rely
on.

**Errors do not leak.** A 500 body never contains a driver message, a DSN, a query
or a host name. Secrets are never logged: not API keys, not DSNs (only datasource
names), not the core API key.

**Requests are rate-limited** per IP and per principal, and every trigger is bounded
by a per-goal cooldown and a per-period budget whose defaults are restrictive and
whose floors cannot be configured away.

**The container is minimal.** Distroless static base, `nonroot` user, no shell, no
package manager, no `.env` in any layer, published ports bound to loopback.

## What core guarantees

Core is the half that actually runs an agent, so its guarantees are about what a run
can reach. Each is enforced in code.

**Privilege is fixed at startup, and there are three kinds of caller.** An
environment key listed in `CORE_API_KEYS` is an operator; one in `CORE_BOT_KEYS` is a
bot; a row in `users` with a session token is a person. No request field can move a
caller between them. A bot cannot become a user, and a user cannot dispatch a system
task.

**Operator-only operations are checked twice**, exactly as in the engine:
`middleware.RequireOperator` on the route and `Actor.requireOperator` in the service.

**Registration is closed by default.** `CORE_OPEN_REGISTRATION=false`, and the first
account is made by `core createuser` on the operator's own shell. A self-hosted box
found on the internet with open sign-up is a box running strangers' code in your
sandbox on your API key. The CLI path goes through the same service and the same
password policy as the route, so it cannot produce a weaker account.

**Passwords are argon2id** with the current OWASP parameters (64 MiB, t=3, p=4), and
the password is read from stdin rather than a flag — a flag is in the process list
and in the shell history. **Session tokens are opaque and stored hashed**, so a
database dump is not a set of live sessions. Sign-in against a missing account still
performs a hash comparison, so the response time does not say whether the address
exists.

**Every query that touches user content is scoped by `user_id` in its `WHERE`
clause**, not filtered afterwards, and each user's sandbox workspace is their own.
One user cannot read, cancel or resume another's run.

**A chat channel is the first place a stranger can reach the agent, and it widens
nothing.** Every connection core makes to Telegram, Slack and Discord is *outbound* —
long polling, a Socket Mode socket, the gateway — so there is no inbound webhook route
on either service, nothing listening for somebody else's signature and nothing to
expose. An inbound message passes seven rungs in a fixed order before it can become a
run, and a sender with no linked account reaches exactly one of them: the check that
tells them to link. No run starts, no tokens are spent, and the reply says the same
thing every time.

**Linking is proved over the channel, not asserted through the API.** There is no route
that attaches an external id to an account. A person mints a code from a signed-in
session and sends it to the bot; the message is the proof. The code is single use,
sha256-hashed in the database, shown exactly once with no route that reads it back, good
for 15 minutes by default with a 1-minute floor and a 1-hour cap, and minting supersedes
whatever that account had outstanding. Unknown, expired and already spent are one answer,
so a sender cannot use the reply to tell live codes from dead ones. A revoked chat account
can be re-claimed only by the account that released it; anybody else is told an operator
has to clear it, and not whose it was.

**A channel run is bounded by the same limits as any other**, and spends the budget of
the person whose chat it is. Shared rooms are refused unless `CHANNEL_ALLOW_GROUPS` is
on, which warns at every startup, because anybody who can type in the room can then spend
that person's daily allowance; even on, only a message addressing the bot is acted on. A
message from another app is never answered, which is what stops two bots paying each
other to talk. A linked sender is held to at most one accepted message a second —
`CHANNEL_MIN_INTERVAL` can lengthen that gap and nothing can shorten it, because zero
would read as "no throttle" on the one decision in core a stranger can drive — and text
longer than a brief may be is refused rather than truncated. The kill switch is read on
this path too, fail-closed.

**Nothing leaks outward on a channel either.** A Discord reply is sent with an empty
allowed-mentions list, so an `@everyone` in model output notifies nobody, and every
adapter's errors are stripped of their token before they reach a log.

**A notification goes to one account's own DMs, and no caller picks it.** `POST
/v1/notifications` takes no recipient. Core resolves the live linked chat identities of
the account `CORE_UNATTENDED_OWNER` names — the same account unattended work is already
filed against — and sends there. Revoked links are skipped. A signed-in person is refused
in the service, the way dispatch already refuses one, so an account on your box cannot
make core message the operator; and the notifier is handed a directory port that can only
list, so the path that sends a message cannot take a link away. The address it resolves is
the person's external *user* id, never the room a message arrived in, so a chat linked
from a shared channel is still notified privately even with `CHANNEL_ALLOW_GROUPS` on.
There is no history behind the route either: `GET /v1/notifications` is a `405`, because a
sent message is not a record and the audit log already holds the decision.

**An agent cannot grant itself a tool.** `config/tools.yaml` is operator-owned, read
once at startup, with no API write path — the same rule the engine applies to metric
definitions. A tool not named there is denied, and the file ships empty: a fresh
instance can think, read its own workspace and answer, and can do nothing else until
somebody writes down what it may do.

**The tool ladder decides every call, in a fixed order.** No grant denies. Switched
off denies. `read` is allowed. `write` is allowed on an attended run and refused on
an unattended one — refused for that run, not queued, because nobody is there to
answer. `spend` with a policy contract goes to the goal engine's existing gate;
`spend` without one is denied. Core has no second, weaker gate of its own.

**Money is never decided in core.** A spending tool files a request with the engine
and takes the answer it gets, `pending` included — and `pending` means a human was
asked, not that the call may proceed.

**The kill switch is checked before every run**, against the engine, with the same
fail-closed rule the engine itself applies: unreadable means engaged, engaged means
the run stops before it starts.

**A run is bounded even if nothing else stops it.** Iterations, tool calls, tokens
per run, tokens per user per day, one model call, one sandbox execution — every one
has a restrictive default and a floor, and an unset value means the default rather
than "no limit". A broken or negative spend ledger stops the run instead of being
read as zero spent.

**The sandbox is a boundary, when you let it be one.** With
`SANDBOX_BACKEND=docker`: no network unless you grant it, a read-only root
filesystem, a tmpfs `/tmp`, `--cap-drop ALL` with `no-new-privileges`, a pid limit, a
memory limit with swap disabled, the host uid rather than root, only the run's own
workspace mounted, and a deadline that removes the container by name if the client
dies first. The child process gets `HOME` and nothing else — none of this process's
secrets.

**Tool configuration holds the names of secrets, never the secrets.** `authEnv` and
`passEnv` take variable names; a value that looks like `TOKEN=abc123` is refused at
startup, and a `baseUrl` carrying `https://user:pass@host` is refused for the same
reason. An MCP server's child process gets a small allowlist plus what you named,
because a denylist means the next secret this service learns to read is the one that
leaks.

**A run's steps are append-only.** There is no repository method that updates or
deletes one. What the agent did is what the transcript says it did.

**Errors do not leak, including the engine's credential.** A failed call to the goal
engine keeps its cause for the log and is asserted by test never to contain the API
key. SQL is parameterised, one repository type per table, no ORM. Requests are rate
limited per IP and per principal.

**The container runs as an unprivileged user**, with no `.env` in any layer. It is
alpine rather than distroless, and [that trade-off is stated
below](#known-limitations) rather than hidden: both sandbox backends need a program
to exist in the image.

## What the console guarantees

`web/` decides nothing. It is a screen over two APIs that re-check every credential
they are handed, and it holds no database, no volume and no state of its own. What it
does hold is credentials, which is the whole of its security story.

**No credential ever reaches a browser.** Three of them exist. The goal engine's *bot*
key sits in the server's environment. A signed-in person's core session token and an
operator's pasted engine key live sealed in httpOnly cookies. All three are read only
inside the Node process: every page is a server component and the interactive pieces are
handed data that was already fetched, `lib/upstream.ts` is the only module that makes an
outbound call, and `publicConfig()` exists so that nothing which might end up in a bundle
has a reason to reach for the config that holds two secrets.

**There is no operator key in its environment, deliberately.** An operator key in a web
server's environment is an operator key that any request-handling bug can spend. Instead
one is pasted, checked before it is held, and sealed for `WEB_OPERATOR_TTL_MINUTES` — 30
by default, a floor of 1, a ceiling of 8 hours, and **it does not renew on activity**.
The status strip shows the countdown, because authority you have forgotten you are
holding is authority somebody else can use on your laptop.

**A pasted key is verified without side effects.** The probe is
`POST /v1/approvals/<a fresh uuid>/resolve`: the engine checks operator authority and
validates the body *before* it looks the approval up, so a random id answers `404` to an
operator key, `403` to a bot key and `401` to one it does not know, having read and
written nothing — no record touched, no audit row. This is the one function in the
console that takes a credential as an argument, and it exists so the general call path
never does.

**Authority is decided in the module, never by the caller.** Each call declares what it
needs and `lib/upstream.ts` picks the credential. A call that declares `operator` when
nobody holds authority is refused locally with a message saying so, rather than quietly
retried with the bot key. One route — `PUT /v1/flags/kill-switch` — sends whichever is
held, which is correct precisely because the engine accepts an engage from anyone and
refuses a release from a bot.

**What a browser can reach is an allow-list, not a proxy.** 36 routes, matched by method
and path shape; anything else is a 404 raised before the code that holds a key is
reached. Three of them can write on the goal engine and each is named in the table with
the authority it needs: resolving an approval and moving a goal's target are operator's,
the kill switch is either. A generic `/api/*` taking a path from a caller would make
every route on both services reachable with whichever credential the console holds, and
that is [on the absent list](docs/roadmap.md#deliberately-absent).

**A cookie value is not a credential.** Each is AES-256-GCM with its purpose bound in as
additional authenticated data, so a session seal cannot be replayed as an operator seal;
the expiry is *inside* the sealed bytes and checked on open, because a `Max-Age` is a
request the client may ignore; and every failure to open — wrong key, tampered bytes,
wrong purpose, expired — returns the same nothing, so there is no decryption oracle. The
cookies are httpOnly, `SameSite=Strict`, `Secure` outside development, and
`WEB_COOKIE_SECRET` must decode to 32 bytes or the console refuses to serve a request.

**A write has to come from this console.** Both cookies being `SameSite=Strict` means a
cross-site request arrives without them; on top of that, any method other than `GET` is
refused unless `Sec-Fetch-Site` is absent or `same-origin`. Bodies are capped at 64 KiB
on the way through, and nothing is cached: `no-store` on every upstream call, because a
cached approval queue is a queue somebody acts on twice.

**Every response carries a per-request CSP nonce.** `script-src` gets the nonce and
`'strict-dynamic'` and never `'unsafe-inline'`, `connect-src` is `'self'` because the
services are never called from the page, and `frame-ancestors`, `base-uri` and
`object-src` are `'none'`. A unit test asserts the nonce is in the header and that
`script-src` has not acquired `'unsafe-inline'`, because a console whose CSP allows
inline script is a console where an injected string can press the button that resolves a
spending decision. `style-src` keeps `'unsafe-inline'` and that exception is stated where
it is made: a stylesheet cannot read a cookie or call an endpoint.

**It fails closed where the services do.** An unreadable kill switch reads as engaged and
the whole console goes to its halted state. A single failed read degrades that panel and
keeps its last value rather than taking the screen down with it, which is the opposite
default from the switch on purpose: one is a missing number, the other is a missing brake.

**It never claims an address on a caller's behalf.** No `X-Forwarded-For` is
synthesised for either service — whether a proxy may claim an address is that service's
`TRUSTED_PROXIES` decision, not the console's. The consequence is
[a limitation below](#known-limitations), not a hidden feature.

**Errors do not leak.** An upstream failure names the service and the timeout, never the
URL, which carries a private address. The configuration error names the variables that
are missing and never their values, and it goes to the console's log rather than the
browser — an anonymous visitor should not be told which credentials the box is short of.

## What it does not

Stated plainly, because a security document that only lists strengths is marketing.

- **It does not defend against a stolen operator key.** An operator can do
  everything. The audit log records what was done; that is the entire mitigation.
- **It does not make an agent trustworthy.** The tool ladder bounds what a run can
  reach; it says nothing about whether reaching it was a good idea. Do not grant
  core credentials you would not give a contractor on their first day.
- **It does not solve prompt injection.** A webpage the agent reads can tell it to
  do anything, and it may try. The defence is that "anything" is a short list: the
  tools you granted, at the class you granted them, inside a sandbox with no network.
  A grant is a decision about what a *confused* agent may do.
- **It does not choose who may message your bot.** On a chat platform, anybody who can
  find it can send it something. What an unlinked sender gets is the linking
  instructions, so the cost is a lookup and a reply rather than a run — but the
  per-sender throttle applies to *linked* senders, and a public bot receiving a flood is
  still a flood. Keep the bot's discoverability down: BotFather's privacy setting on,
  the Slack app installed where you meant it, the Discord invite not shared.
- **It does not keep a channel conversation private from the platform.** A message
  typed into Telegram, Slack or Discord is in their hands before it is in yours, and the
  reply goes back the same way. Anything you would not put in a third party's logs does
  not belong in a chat.
- **It does not hide that something happened.** A notification is a chat message, so
  turning them on tells a third party that a goal on your instance fell behind, or that a
  spend of some kind is waiting, along with the id of the thing and the hostname of your
  console. The figures stay here; the fact does not. If even that is too much, leave
  `NOTIFY_ENABLED=false` and read the deck.
- **It does not guarantee you were told.** Delivery is best effort by design: one
  attempt, no queue, no retry, and a failure that is logged rather than raised. An
  unanswered approval still expires on its own, which is the safety net that matters, but
  do not build a process that assumes silence means nothing happened.
- **It does not validate that a metric is *true*.** A `push` metric is whatever its
  credential says it is.
- **It does not encrypt anything at rest.** The databases hold your metrics, audit
  trail, chats and run transcripts in plain tables. Use disk encryption and a backup
  process you trust.
- **It does not authenticate core's response.** A compromised core can lie about
  having accepted a task. It cannot change a decision, because no decision is made
  there.
- **It does not make the console safe to publish.** It listens on plain HTTP and has
  no TLS of its own; the documented deployment is loopback with the operator's own TLS
  in front. Its cookies are `Secure` outside development, which means served over plain
  HTTP to anything but localhost they are not stored at all and sign-in appears to loop.
- **It does not defend a browser that is already running hostile script on the
  console's origin.** httpOnly cookies and a nonce-based CSP are there to make that
  unlikely, and they do mean such a script cannot *take* the credentials anywhere — but
  a same-origin `fetch` is indistinguishable from a click, so anything on that origin
  can do what the signed-in person could do. That is why the console's forwarded surface
  is a short list and why holding operator authority is meant to be brief.
- **It does not give the console a decision of its own.** A compromised console can
  engage the kill switch — anyone may — and act as whoever's cookie it holds. It cannot
  move a ladder, raise a cap, define a metric, grant a tool, or alter an audit row,
  because none of those live there.
- **It does not stop an agent doing damage that costs nothing.** The approval gate
  is about money. An agent that sends a regrettable email is bounded only by the
  class you gave that tool and by whether the run was attended.
- **It has no vulnerability-disclosure history**, because it has no production
  history. Treat the guarantees above as claims that have been reasoned about and
  unit-tested, not as claims that have survived contact with an attacker.

## Hardening a deployment

In rough order of how much each one buys you:

1. **Separate keys per caller, named after who they are.** One operator key for you,
   one bot key per agent, one bot key per metric feed. An audit trail that can only
   say "some valid key" does not answer "who moved that target".
2. **Grant tools one at a time, at the honest class.** `config/tools.yaml` ships
   empty, and every entry you add is a decision about what a *confused* agent may do.
   Classifying something as `read` because that is more convenient is how a wrong
   class becomes a wrong action.
3. **Run `SANDBOX_BACKEND=docker` with `SANDBOX_NETWORK=none`.** `local` is for a
   laptop without a daemon and a single-operator box where the agent's tools are
   already your tools; it is not a boundary. Never set it inside core's own
   container — a generated script would then run beside your API keys and the docker
   socket.
4. **Read-only database roles for `sql` metrics.** The engine's read-only
   transaction is a guarantee about the engine, not about the credential.
5. **Do not publish the ports.** The compose file binds all three to `127.0.0.1`. If
   your agents run on the same host, leave them there. Docker writes its own iptables
   rules and will happily bypass a ufw that says otherwise.
6. **Put TLS in front of the console, and leave it on loopback.** It serves plain HTTP
   and has none of its own, and its cookies are `Secure` outside development — over
   plain HTTP to anything but localhost the browser stores nothing and sign-in appears
   to loop. A reverse proxy terminating TLS is the deployment; `ssh -N -L
   3000:127.0.0.1:3000` is the answer for one operator on one laptop. Do not let that
   proxy set its own Content-Security-Policy: overwriting the console's nonce leaves a
   page that renders and whose every button is dead.
7. **Give the console a bot key and never an operator key.** `GOAL_ENGINE_BOT_KEY` is
   the only engine credential it should have; an operator pastes theirs when they need
   it, and a shorter `WEB_OPERATOR_TTL_MINUTES` than the 30-minute default costs one
   extra paste. Generate `WEB_COOKIE_SECRET` with `openssl rand -base64 32`, keep it out
   of the image, and know that rotating it signs everybody out and drops any held
   authority — which is also how you revoke both in a hurry.
8. **Leave `CORE_OPEN_REGISTRATION=false`** and make accounts yourself with
   `core createuser`. If you do turn it on, put it behind something that rate limits
   harder than core does.
9. **Leave `CHANNEL_ALLOW_GROUPS=false`** and keep the bot hard to find: BotFather's
   privacy setting on, the Slack app installed only where you meant it, the Discord
   invite not shared. In a shared room, a run started by anybody who can type there is
   filed against the person who linked the chat and spends *their* daily allowance.
10. **Set `TRUSTED_PROXIES` to your proxy and nothing else.** Empty behind a proxy
    means every caller shares one rate-limit bucket. Too broad means a client can
    send its own `X-Forwarded-For` and choose its bucket.
11. **Keep `TRIGGER_DRY_RUN=true` until you have read a week of decisions.** The
    cheapest security control in the project is not letting it act yet.
12. **Add spending policies one action type at a time**, with a small
    `autoApproveBelow`. The empty default is correct; do not fix it before you have
    seen real requests.
13. **Pin every MCP server to a version.** `npx -y some-package` runs whatever that
    name resolves to today, in a process that holds the variables you named in
    `passEnv`.
14. **`chmod 600` every env file** — the stack's, both services', and the console's —
    and back them up encrypted and out of git.
15. **Back up both databases.** They are your record of what the agent did. The console
    has nothing to back up, which is one of the nicer things about it.
16. **Rotate keys by adding, restarting, moving the caller, removing, restarting.**
    Both are valid in between, so nothing has an outage.
17. **If you turn notifications on, treat `WEB_BASE_URL` as public.** Every message
    carries it, so that hostname ends up in Telegram's, Slack's or Discord's message
    store whether or not the console is reachable from there. That is an argument for a
    name behind your own TLS and your own access, not for leaving the link out — a
    notification with nowhere to go is a prompt to look at something without being told
    where.

## Known limitations

Design trade-offs, documented rather than hidden. None is a vulnerability report;
all of them are places a reader might reasonably expect something stronger.

**Prompt injection is bounded, not prevented.** Anything the agent reads can instruct
it, and there is no filter here that claims to catch that. What limits the damage is
the size of the list: the tools you granted, at the class you granted them, in a
sandbox with no network by default, under caps that stop a loop. A grant is the
security control; the model's judgement is not.

**The `local` sandbox backend is not isolation, and says so.** It runs the script as
this service's own user. It gets a minimal environment with none of this process's
secrets and a deadline that kills it, but a script that reads `~/.ssh/id_rsa` will
read it, and on Linux the same uid can read this process's own environment. Use
`docker` on anything that matters.

**The docker sandbox backend is a boundary, not a wall.** It is a shared-kernel
container with `--cap-drop ALL`, `no-new-privileges`, a read-only root filesystem, no
network by default and a pid limit — which stops the ordinary cases, not a kernel
exploit. If you need a wall, run core on a VPS that holds nothing else; that is why
the backend is a port.

**Core's container needs a program in it, so it is not distroless.** Both backends
look up a binary at startup — `sh` for `local`, the `docker` CLI for `docker` — so a
distroless core would refuse to boot rather than fail mysteriously later. The image
is alpine with `ca-certificates` and `docker-cli`, running as an unprivileged fixed
uid. The goal engine, which executes nothing, stays distroless.

**Mounting the docker socket is trusting the host's daemon.** The docker backend asks
the *host's* daemon to start a sibling container, so core's container needs the socket
and the socket's group. Access to that socket is equivalent to root on the host. The
alternative — docker-in-docker — trades it for a privileged container, which is worse.
If this is not acceptable, run core with `SANDBOX_BACKEND=local` on a machine that
holds nothing else, and accept the previous limitation instead.

**On Windows the sandbox cannot kill a detached grandchild.** A timed-out script's
own process is killed; a process it started that detached itself may outlive it.
Linux gets a process group and does not have this problem. Windows is a development
platform here, not a deployment one.

**Push metrics are self-reported.** A credential that can report `business.mrr` can
report any number for it. A goal judged on a fabricated value behaves exactly as if
the value were real. Mitigations: one key per feed, so a wrong number has one owner;
`observedAt` may not be more than 5 minutes ahead or 7 days behind, so a
clock-skewed or replayed batch cannot rewrite history; and a value older than
`METRIC_MAX_SAMPLE_AGE` stops evaluation rather than reading as on-track. There is
no cryptographic feed attestation, and adding one would not change the fact that the
feed computes the number.

**An audit write that fails does not roll back the action.** A spend already
approved is not un-approved by a failed insert. The failure is logged loudly. This
is the one place the audit log can be incomplete, and the service log is the
backstop. The alternative — refusing an approved action because a log write failed —
was judged worse.

**Config changes need a restart.** Every YAML file in both services is read once at
startup: the engine's metrics and policies, core's tools, MCP servers and HTTP
services. That is deliberate — a limit that can change without a deploy is a limit
somebody can change quietly. It also means a compromised host with write access to
the config and the ability to restart the process can change your limits, at which
point the host is the problem.

**A run's tool set is fixed when it starts.** An operator who removes a grant mid-run
does not remove it from a run already in flight; the next run sees the change. The
alternative — a run whose permissions shift underneath it — is harder to reason about
after the fact.

**`sql` metric queries are operator-supplied SQL.** They are checked to be a single
read and run in a read-only transaction, but an operator who writes a slow query has
written a slow query. `METRIC_SAMPLE_TIMEOUT` bounds it.

**Rate limiting is in-process, and the console counts as one caller.** Fine for one
instance of each service, which is the documented deployment; two instances behind a load
balancer each get their own bucket. And because the console calls both services from its
own process and deliberately does not synthesise an `X-Forwarded-For`, each service sees
*its* address for every browser behind it: the per-IP bucket is shared by everyone using
the console. What still separates them is the per-principal limit — each person's core
session is its own principal — while every console read of the goal engine carries the
same bot key and therefore lands in the same bucket. The console adds no limit of its
own; a signed-in person hammering a forwarded route is bounded by core's limits, not by
the console's.

**CSRF is the console's problem, and neither API has the surface.** Both services take a
bearer token or `X-API-Key` and never a cookie — core's session token is returned to the
client to send as a bearer token, not set as one. The console is the one place where a
cookie authenticates a write, and it defends that with two checks and no token: both
cookies are `SameSite=Strict`, so a cross-site request arrives without them, and any
method other than `GET` is refused unless `Sec-Fetch-Site` is absent or `same-origin`. The
absent case is allowed on purpose, because that is `curl` against a local instance, which
has to send a credential of its own regardless. A double-submit token would be a third
check on a surface where the first two already fail closed. If you put your *own* browser
UI in front of either service, its session handling is still your responsibility.

**The console's screens are not tested, only its logic.** The suite covers the sealing,
the route table, the envelope parser, the CSP and the arithmetic — 229 tests, no browser
and no database. A component that arranges what a server component already fetched has
little to assert that `tsc` does not, so "this button resolves the approval it is next to"
is proved by types and by the build rather than by a test. The two files that behave like
the Go ladders are `proxy-routes.test.ts` and `csp.test.ts`, and those are the ones to
read first if you are reviewing this part.

**Nothing is encrypted at rest, including run transcripts.** A run's steps hold
whatever the agent read, which for a tool that queries your database means business
data in core's tables. Disk encryption and a backup process you trust are the answer.

**The race detector runs only in CI.** `make test-race` needs cgo and a C toolchain,
so it is a separate target from `make test`. CI runs it on every push for both Go
modules; nothing merges unraced. The console has no equivalent — CI type-checks, tests
and builds it on Node 22, which is what there is to run.
