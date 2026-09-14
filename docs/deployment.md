# Deployment

Self-hosting Wingman on one VPS, from an empty box to the first trigger. Roughly
40 minutes, plus a week of watching before you let it act.

- [What you need](#what-you-need)
- [1. The box](#1-the-box)
- [2. Clone](#2-clone)
- [3. Configure](#3-configure)
- [4. Start](#4-start)
- [5. Verify](#5-verify)
- [6. Put a reverse proxy in front](#6-put-a-reverse-proxy-in-front)
- [7. Declare your metrics](#7-declare-your-metrics)
- [8. Grant core its first tool](#8-grant-core-its-first-tool)
- [9. Spend a week in dry run](#9-spend-a-week-in-dry-run)
- [10. Turn on spending, narrowly](#10-turn-on-spending-narrowly)
- [Operating it](#operating-it)
- [Backups](#backups)
- [Upgrades](#upgrades)
- [Running without Docker](#running-without-docker)
- [Troubleshooting](#troubleshooting)

## What you need

| | |
|---|---|
| A VPS | 2 vCPU / 4 GB runs both services, both databases and the console. Size for the sandbox: every concurrent run starts a container, so `RUN_WORKERS × SANDBOX_MEMORY` is the memory that is not yours. |
| Docker | Engine 24+ with the Compose plugin. Core also needs the daemon at *runtime*, not only to build — that is what a sandbox is. |
| A domain | Only if you want to reach it from anywhere but the box. In normal use that means the console; neither API has to be reachable at all. |
| An LLM API key | Anthropic or OpenAI, for core. The goal engine calls no model and needs no key. |
| A sandbox image | Whatever toolchain the work needs — `alpine:3.22`, `python:3.13-alpine`, or your own. Nothing publishes `wingman/sandbox`. |

Five containers: two Go binaries, the Next.js console, and two Postgres databases. The
stack is one compose file in `deploy/`.

**Read this before choosing the box.** Core is handed `/var/run/docker.sock` so it
can start sandbox containers, and access to that socket is root-equivalent on the
host. Do not put anything else on this machine that you would mind an agent
reaching. [SECURITY.md](../SECURITY.md#known-limitations) states the limitation
plainly rather than talking around it.

## 1. The box

```bash
# Docker, from Docker's own repository rather than the distro's.
curl -fsSL https://get.docker.com | sh

# Deny inbound by default. Note the caveat below.
sudo ufw default deny incoming
sudo ufw allow OpenSSH
sudo ufw allow 80,443/tcp
sudo ufw enable
```

**Docker bypasses ufw.** Docker writes its own iptables rules, so a container
published as `8080:8080` is reachable from the internet no matter what ufw says.
This is why `deploy/docker-compose.yml` publishes to `127.0.0.1:8080`,
`127.0.0.1:8081` and `127.0.0.1:3000` instead. If you change any of those bindings,
you have opened a port, and ufw will not tell you. The console is the one you will
want reachable, and [step 6](#6-put-a-reverse-proxy-in-front) is how — through a
proxy that terminates TLS, not by widening the binding.

## 2. Clone

```bash
git clone https://github.com/ribdsp/wingman.git
cd wingman
```

Everything is in this repository — `goal-engine/` and `core/`, two Go modules, plus
`web/`, the console — two databases, one compose file. There is nothing to provision
from anywhere else.

The sandbox workspace has to exist before the first `up`, owned by the uid core runs
as. The image builds an unprivileged user at a fixed 65532 precisely so this is
predictable:

```bash
sudo mkdir -p /srv/wingman/workspaces
sudo chown 65532:65532 /srv/wingman/workspaces
```

## 3. Configure

Four environment files, not one:

```bash
cd deploy
cp .env.example                 .env               # the stack's own settings
cp ../goal-engine/.env.example  .env.goal-engine   # the engine's keys
cp ../core/.env.example         .env.core          # core's keys
cp ../web/.env.example          .env.web           # the console's
chmod 600 .env .env.goal-engine .env.core .env.web
$EDITOR .env .env.goal-engine .env.core .env.web
```

The split is the same property the two services exist to have. The goal engine holds
the operator keys that approve spending; core and the console are clients of that API
and are the things that ask. One shared file mounted into all three containers would
put the approving credential inside the service running model output *and* inside the
web server, and then breaking into either of them would be breaking into all.

`deploy/.env` is read by compose itself — to name the databases, build each
`DATABASE_URL`, and decide which host ports to publish. It is handed to no
container.

### Eight secrets, none of them reused

```bash
openssl rand -hex 24      # run this seven times
openssl rand -base64 32   # once more, for the cookie key: it must be exactly 32 bytes
```

| File | Value | Is |
|---|---|---|
| `.env` | `POSTGRES_PASSWORD` | the engine's database |
| `.env` | `CORE_POSTGRES_PASSWORD` | core's database |
| `.env.goal-engine` | `GOAL_ENGINE_API_KEYS=ops:…` | **you**, on the engine |
| `.env.goal-engine` | `GOAL_ENGINE_BOT_KEYS=wingman-core:…` | what core signs in with |
| `.env.goal-engine` | `GOAL_ENGINE_BOT_KEYS=web-console:…` | what the console reads with |
| `.env.core` | `CORE_API_KEYS=ops:…` | **you**, on core |
| `.env.core` | `CORE_BOT_KEYS=goal-engine:…` | what the engine dispatches with |
| `.env.web` | `WEB_COOKIE_SECRET` | what seals the console's two cookies |

The two bot entries in the engine's list are one comma-separated variable and two
distinct secrets. Sharing one is not a shortcut the engine allows — startup refuses
two principals behind the same secret — and the reason it refuses is the audit log:
"who read the queue" and "what core did while working" should not resolve to the same
name.

Three of those values appear twice, because they are how the three services reach each
other. The secret has to match on both sides, and the *list* it sits in is what decides
privilege:

```bash
# .env.goal-engine — what the engine sends to core. Must be the secret from
# core's CORE_BOT_KEYS. A bot there: it files tasks, it does not run core.
WINGMAN_CORE_API_KEY=<the secret in .env.core's CORE_BOT_KEYS>

# .env.core — what core sends to the engine. Must be the secret from the engine's
# GOAL_ENGINE_BOT_KEYS. A bot there: it asks to spend, it does not approve.
GOAL_ENGINE_API_KEY=<the secret in .env.goal-engine's GOAL_ENGINE_BOT_KEYS>

# .env.web — what the console reads the engine with. The OTHER bot secret, the
# web-console one. A bot there too: it may read every screen and engage the kill
# switch, and it may not release one, resolve an approval or move a target.
GOAL_ENGINE_BOT_KEY=<the web-console secret in GOAL_ENGINE_BOT_KEYS>
```

Neither cross-service credential is an operator key on the other side, and that is
not tidiness. If core held an operator key on the engine, the service running
agent-authored output could resolve its own approval requests, and the gate would be
decorative. The console is the same argument one layer out: a long-lived operator key
in a web server's environment is one request-handling bug away from being an operator,
so there is no `GOAL_ENGINE_OPERATOR_KEY` to set — you paste yours when you need it,
and it lives in your own cookie for half an hour.

Note what is **not** in the table: no key for the console to reach core with. It
reaches core as whoever is signed in, using their session token and nothing else.

Keys are `name:secret`. The name is what the audit log records, so name them after
who they are — an audit trail that can only say "some valid key" does not answer
"who moved that target". A secret is at least 24 characters and must not contain a
colon.

Which list a key is in is the **only** thing that decides what it can do:

- `GOAL_ENGINE_API_KEYS` — you. Resolve approvals, edit goal targets, release the
  kill switch.
- `GOAL_ENGINE_BOT_KEYS` — agents, core, and metric feeds. State goals, read, report
  metric values, ask to spend. Nothing in the list above.
- `CORE_API_KEYS` — you. Cancel somebody else's run, read somebody else's
  transcript, make accounts.
- `CORE_BOT_KEYS` — the engine's trigger bridge. File a task and read what it filed.
  Cannot read a person's chats.

A human user is in neither of core's lists. People live in core's database, and no
environment variable can produce a user session. The console is the same: it holds a
bot key for the engine and no key at all for core, so everything it shows you out of
core is read with your own session token.

Give every metric feed its own bot key rather than reusing the one an agent thinks
with. A reported number is exactly as trustworthy as the credential that sent it.

### What else has to be set in `.env.core`

```bash
DEFAULT_PROVIDER=anthropic
DEFAULT_MODEL=              # no default in code, on purpose — see below
ANTHROPIC_API_KEY=

CORE_UNATTENDED_OWNER=you@example.com    # step 5 makes this account
SANDBOX_IMAGE=alpine:3.22                # replace the placeholder
```

`DEFAULT_MODEL` ships empty deliberately. Vendors retire model ids on their own
schedule, and a constant baked into a release fails at the first model call with a
message about the vendor rather than about this file. Put the id you are paying for.

`CORE_UNATTENDED_OWNER` is the account that work nobody asked for is filed against
and billed to. It is required as soon as `CORE_BOT_KEYS` is set and has no default:
a goal-engine trigger has no person behind it, and a run with no owner is a run
whose tokens appear in no ledger and whose transcript nobody can find. It is
resolved to an account id once at boot, so a typo stops the process rather than the
first dispatch — which means the account has to exist first, and step 5 makes it.

`SANDBOX_IMAGE` defaults to `wingman/sandbox:latest`, which nothing publishes. It is
a filesystem rather than an application here: the entrypoint is replaced with `sh`,
so whatever the image declares is not what runs.

### What else has to be set in `.env.web`

Two values, and the rest have defaults:

```bash
GOAL_ENGINE_BOT_KEY=            # the web-console secret from GOAL_ENGINE_BOT_KEYS
WEB_COOKIE_SECRET=              # openssl rand -base64 32 — exactly 32 bytes

WEB_OPERATOR_TTL_MINUTES=30     # floor 1, ceiling 480
WEB_POLL_INTERVAL_MS=4000       # floor 1000
WEB_UPSTREAM_TIMEOUT_MS=15000   # floor 1000
```

The console validates its environment the first time a request needs it, collects every
problem and answers with all of them named at once — the same rule both Go services
follow, and for the same reason: a console that half-works is worse than one that does
not, because it looks like it is working. A missing key does not become an empty
approval queue; it becomes an error that says which variable is missing. `CHANGE_ME`
counts as unset.

`WEB_COOKIE_SECRET` seals two things: a signed-in person's core session token and a
pasted operator key. Rotating it signs everybody out, which is the correct behaviour
for a secret whose only job is to make old cookies unreadable.

The base URLs are set by compose; there is nothing else to fill in.
[docs/web.md](web.md#settings) is the full surface, including what the console
deliberately cannot reach.

### Chat platforms, if you want any

All four credentials ship empty, and empty means off. An instance with none set has no
inbound chat path at all — it is reached over HTTP and by the goal engine, which is the
default and a complete installation.

```bash
CHANNEL_TELEGRAM_TOKEN=          # from @BotFather; leave its privacy setting on
CHANNEL_SLACK_BOT_TOKEN=         # xoxb-…, talks to the Web API
CHANNEL_SLACK_APP_TOKEN=         # xapp-…, opens the Socket Mode socket
CHANNEL_DISCORD_TOKEN=           # a bot token, with the MESSAGE CONTENT intent enabled

CHANNEL_LINK_CODE_TTL=15m        # 1m floor, 1h cap
CHANNEL_MIN_INTERVAL=1s          # also the floor; there is no way to ask for none
CHANNEL_ALLOW_GROUPS=false
```

`core/.env.example` lists what each platform has to be told on its own side — the Slack
scopes and event subscriptions, Discord's privileged intent, BotFather's privacy setting.

**Nothing has to be opened.** Every connection is outbound — Telegram long polling,
Slack's Socket Mode socket, Discord's gateway — so this needs no public address, no
certificate, no forwarded port and no route that verifies somebody else's signature. A box
behind a home router works, and there is nothing to put in the reverse proxy of step 6.

Three refusals at startup, each costing a line here instead of a `401` to look up later:
a `CHANNEL_TELEGRAM_TOKEN` that is not shaped like one, one Slack token without the other,
and the same value pasted into both Slack fields. A well-formed token the platform itself
rejects cannot be caught here — it fails on the identify call that adapter makes before it
reads anything, and the other channels and every HTTP route carry on.

`CHANNEL_ALLOW_GROUPS` is the one to think about. Off, core acts only on direct messages.
On, it acts on a shared-room message that addresses the bot — and that run is filed
against whoever linked the chat and spends **their** daily allowance, so anybody who can
type in the room can spend it. Turning it on logs a warning every time core starts.

`CHANNEL_MIN_INTERVAL` is the gap enforced between two accepted messages from one sender.
Its default is its floor: zero would read as "no throttle", and this is the only decision
in core that a stranger can drive.

A chat account does nothing until somebody links it. Signed in, they `POST
/v1/channels/link-codes` and send the code to the bot on its own; it is good once, and only
for `CHANNEL_LINK_CODE_TTL`. [core.md](core.md#channels) has the rest, including the seven
rungs an inbound message passes before it becomes a run.

### Notifications, if you want them

Off by default. Turned on, the goal engine tells you on a chat platform you already use
when a goal fell behind and an agent was woken, and when a spend is waiting for your
decision. Three variables, all in `.env.goal-engine`:

```bash
NOTIFY_ENABLED=false                    # one line turns it on
WEB_BASE_URL=                           # https://wingman.example — where the console is
# WINGMAN_CORE_NOTIFY_PATH=/v1/notifications   # only if core moved the route
```

`WEB_BASE_URL` is what builds the link in the message, and it is the whole point of the
message: a notification carries **no amount, no currency and no pace figure**, so the link
is what gets you to the console where those are. It must start with `http://` or `https://`
or the engine refuses to boot. Unset is allowed and means the message goes out without a
link rather than with a broken one — but then you are being told to go and look at
something without being told where, so set it.

`WINGMAN_CORE_NOTIFY_PATH` exists for the same reason `WINGMAN_CORE_TASK_PATH` does: the
two services deploy independently, so core moving its route should be a config edit here
rather than a redeploy of both.

Nothing is needed in `.env.core`. The engine posts one line to core, and core sends it to
**whichever chat accounts `CORE_UNATTENDED_OWNER` has linked** — the same account
unattended work is already filed against. That is deliberate: there is no recipient in the
request and no "who to notify" setting in either service, because a field naming who to
message would let anything holding a bot key send a message from your instance to any chat
account linked to it.

Which means one manual step, once, and it is the step people miss:

1. Configure at least one chat platform above.
2. Sign in to the console (or core directly) **as the account `CORE_UNATTENDED_OWNER`
   names** — step 5 makes it.
3. `POST /v1/channels/link-codes` and send the code to the bot in a direct message.
4. Cause something worth telling you about, or just watch the engine's log: it says
   `notification sent` with the kind and the subject id, and it says `could not notify` when
   nothing got through.

Until that link exists, notifications are enabled and go nowhere. Core answers `200` with
`recipients: 0` and the message "Nobody was notified: no chat account is connected to the
unattended owner", and the engine logs it — that line is the symptom to look for.

Best effort, on purpose: nothing is retried, nothing is queued, no table is added to either
database, and a failed delivery cannot affect a decision. A lost message costs you a
prompt; the approval is still in the queue, the console still shows it, and its
`APPROVAL_TTL` still expires it.

### What compose sets for you

Do not set these in the service files; the compose file overrides them, and a value
copied from a developer's machine would point at a localhost that means the
container itself:

| | |
|---|---|
| `DATABASE_URL` | inside this network the databases are `goal-db` and `core-db` |
| `PORT` | 8080 for the engine, 8081 for core, 3000 for the console |
| `WINGMAN_CORE_BASE_URL` | `http://core:8081` |
| `GOAL_ENGINE_BASE_URL` | `http://goal-engine:8080` — for core, and for the console |
| `CORE_BASE_URL` | `http://core:8081` — the console's other half |
| `HOSTNAME` | `0.0.0.0`, for the console only: a process bound to loopback *inside* a container is unreachable even from its own published port. The loopback binding that matters is on the host side, and compose does that one |
| `SANDBOX_WORKSPACE_ROOT` | the host path from `CORE_WORKSPACE_DIR`, identical inside and out |

That last one is the one people get wrong. With `SANDBOX_BACKEND=docker`, core asks
the **host's** daemon for a sibling container, and the daemon resolves a bind mount
against the host's filesystem. If core says `/app/workspaces` and the host has no
such directory, the sandbox gets an empty `/workspace` and the agent quietly finds
nothing. Same path on both sides, or nothing works and nothing errors.

One more, in `deploy/.env`:

```bash
CORE_DOCKER_GID=999      # stat -c '%g' /var/run/docker.sock
```

Membership of that group is how core reaches the daemon without running as root. It
is also root-equivalent on the host. Both halves of that sentence are true at once.

Leave `TRIGGER_DRY_RUN=true`. Step 9 is about why.

## 4. Start

```bash
docker compose up -d --build
docker compose ps
```

Each database becomes healthy first; each service waits for its own, because both
apply migrations on boot (`AUTO_MIGRATE=true`) and neither can do that against a
database that is still initialising. Nothing between the two services is published
to the host — they reach each other by service name on the compose network.

The console starts after both but waits for neither to be *healthy*, on purpose. Its
job when a service is down is to say which one and why; a dashboard that refuses to
start until the thing it monitors is up is a dashboard you cannot use to find out why it
is down.

## 5. Verify

```bash
curl -s localhost:8080/readyz | jq -c .data
# {"killSwitchEngaged":false,"metrics":4,"status":"ready"}

curl -s localhost:8081/readyz | jq -c .data
# {"status":"ready"}

curl -s -o /dev/null -w '%{http_code}\n' localhost:3000/signin
# 200 — the console is serving; it has no readyz because it has nothing to be ready for
```

Both service answers are the `data` of the shared envelope. `metrics: 4` is the four push
metrics that ship in `goal-engine/config/metrics.yaml`. If it says `0`, the config
volume did not mount.

Core's readiness deliberately says nothing about the goal engine being reachable or
the kill switch being released. An instance whose engine is down must still serve
the routes a person uses to see what their agent did, and an engaged switch was
engaged on purpose.

### Core's first account

Registration is closed by default, so this is the way in:

```bash
read -rs PW && printf '%s' "$PW" | docker compose exec -T core \
  /app/core createuser -email you@example.com -name 'Your Name'
```

The password is read from standard input rather than a flag, so it stays out of the
process list and the shell history. Use the address you put in
`CORE_UNATTENDED_OWNER`, then `docker compose restart core` if you set that variable
before the account existed — it is resolved at boot.

Then sign in and ask the agent something:

```bash
TOKEN=$(curl -s localhost:8081/v1/auth/signin -H 'Content-Type: application/json' \
  -d '{"email":"you@example.com","password":"'"$PW"'"}' | jq -r .data.token)

curl -s localhost:8081/v1/messages -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"text":"summarise what you can and cannot do right now"}' | jq .data
```

That returns a chat, your message and a queued task. A worker claims it within
`RUN_QUEUE_POLL`; read what happened:

```bash
RUN=$(curl -s localhost:8081/v1/tasks/<taskId>/runs -H "Authorization: Bearer $TOKEN" \
  | jq -r '.data[0].id')

curl -s localhost:8081/v1/runs/$RUN        -H "Authorization: Bearer $TOKEN" | jq .data.stop
curl -s localhost:8081/v1/runs/$RUN/steps  -H "Authorization: Bearer $TOKEN" | jq .data
curl -s localhost:8081/v1/runs/$RUN/cost   -H "Authorization: Bearer $TOKEN" | jq .data.total
```

The first run on a fresh instance will answer and call nothing, because
`config/tools.yaml` ships granting nothing. That is step 8.

The rest of this page uses two more shell variables. Set them now:

```bash
export OP=<your goal-engine operator secret>       # from GOAL_ENGINE_API_KEYS
export CORE_OP=<your core operator secret>         # from CORE_API_KEYS
```

Two different secrets on two different services. An operator key on one is nothing
on the other, which is why there are two.

### Signing in to the console

Everything below can also be done by clicking, and after the first walkthrough that is
how you will do it. The console is on `127.0.0.1:3000` — from your laptop, over the SSH
connection you already have:

```bash
ssh -N -L 3000:127.0.0.1:3000 you@your-box
```

Then <http://127.0.0.1:3000> and the account you just made. The first screen is the five
things worth seeing at once: the open approval queue, every goal's pace as a single
number, what the last tick decided, the most recent audit entries, and a chat. Across the
top is a strip that says live or halted, carries the kill switch, and counts what is
waiting, what today has cost, and how long your operator authority has left. Four more
screens are in the rail for when one of those needs pursuing.

Two things are worth knowing before you rely on it.

**Its own key is a bot key**, so out of the box it can read every screen and engage the
switch, and it cannot resolve an approval, release the switch or move a target. Those
three ask you to paste your operator secret — the `$OP` above — on `/authority`. It is
checked against the engine, sealed into your own cookie, and expires in
`WEB_OPERATOR_TTL_MINUTES` with the remaining time on screen. There is no renewal: when
it lapses you paste it again, which is the point of a short one.

**It polls, and it fails soft.** Each panel reads its own data and says so when a read
fails, rather than blanking the page — except the kill switch, which reads as *engaged*
when it cannot be read at all. A console that cannot tell you whether everything is
halted tells you the safe answer.

[docs/web.md](web.md) is the rest: every screen, the credential model, and what the
console deliberately cannot reach — no metric definitions, no tool grants, no spending
policy, no task dispatch.

### The engine, and one goal end to end

```bash
# Auth works, and the enums a dashboard needs.
curl -s localhost:8080/v1/reference -H "Authorization: Bearer $OP" | head -c 400

# An unauthenticated call is refused.
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/v1/goals    # 401
```

```bash
# State it.
curl -s localhost:8080/v1/goals -H "Authorization: Bearer $OP" \
  -H 'Content-Type: application/json' \
  -d '{"product":"acme","title":"MRR to 50M by end of Q4",
       "sourceText":"get MRR to 50M before the quarter ends",
       "metricKey":"business.mrr","targetValue":50000000,
       "periodEnd":"2026-12-31T23:59:59Z"}'

# Report a value well behind pace.
curl -s localhost:8080/v1/metrics/business.mrr/samples -H "Authorization: Bearer $OP" \
  -H 'Content-Type: application/json' -d '{"value":31400000}'

# Look now, instead of waiting for MONITOR_INTERVAL.
curl -s -XPOST localhost:8080/v1/monitor/tick -H "Authorization: Bearer $OP"
```

The tick response tells you what it decided. With `TRIGGER_DRY_RUN=true` a
`trigger` decision is recorded and audited but no task reaches core — which is
exactly what you want to see before it does.

```bash
curl -s "localhost:8080/v1/audit?limit=10" -H "Authorization: Bearer $OP"
```

## 6. Put a reverse proxy in front

For the console, if you want it from anywhere but an SSH tunnel — and for either API,
only if something off this box has to call one. If your browser, your agents and your
feeds are all on this host, you need none of this.

```nginx
server {
    listen 443 ssl http2;
    server_name wingman.example.com;

    ssl_certificate     /etc/letsencrypt/live/wingman.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/wingman.example.com/privkey.pem;

    # The console. The only one of the three a person needs.
    location / {
        proxy_pass http://127.0.0.1:3000;
        include /etc/nginx/wingman-headers.conf;
        # Next streams a page in pieces as the server finishes each read. Buffering the
        # response holds the whole thing back and turns a fast first paint into a wait.
        proxy_buffering off;
    }

    # The goal engine: goals, approvals, the audit log, the kill switch.
    location /engine/ {
        proxy_pass http://127.0.0.1:8080/;
        include /etc/nginx/wingman-headers.conf;
    }

    # Core: chats, runs, accounts.
    location /core/ {
        proxy_pass http://127.0.0.1:8081/;
        include /etc/nginx/wingman-headers.conf;
    }
}
```

The console does not go through this to reach the two services — it calls them by service
name on the compose network. Exposing an API here is for your scripts, your metric feeds
and anything else outside the box, and each of them still needs its own key.

**Serve the console over TLS, not plain HTTP.** Its cookies are set `Secure` in
production, and a browser will not send a `Secure` cookie over `http://` to anything but
`localhost`. Reached as `http://your-box:3000` from a laptop, sign-in appears to succeed
and every page then redirects back to the sign-in screen, because the cookie was set and
never sent. Over the SSH tunnel in step 5 it works, because there the origin *is*
localhost.

```nginx
# /etc/nginx/wingman-headers.conf
proxy_set_header Host              $host;
proxy_set_header X-Real-IP         $remote_addr;
proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
proxy_set_header X-Forwarded-Proto $scheme;
```

Then set `TRUSTED_PROXIES=127.0.0.1` in **both** service files:

```bash
# .env.goal-engine and .env.core
TRUSTED_PROXIES=127.0.0.1
```

This one matters in both directions. Left empty behind a proxy, every caller shares
one rate-limit bucket, because they all appear to come from the proxy. Set too
broadly, a client can send its own `X-Forwarded-For` and pick its bucket. List the
proxy, and nothing else.

All three speak plain HTTP on purpose — TLS belongs to the thing that already
has your certificates and renews them.

The console needs no `TRUSTED_PROXIES` of its own, because it rate-limits nothing: the
limiter that matters for sign-in is core's, and core sees the console's address for every
attempt. That bounds the total attempt rate — which, with argon2id, is what stops
password guessing — but it does mean everyone signing in through the console shares one
bucket. A console reachable from the internet belongs behind whatever you already use to
keep strangers off things.

Think about whether either API needs to be published at all. Core and the console both
reach the engine over the compose network; the only reason to expose it is your own
`curl`, or a metric feed that runs somewhere else. Publishing the service that resolves
approvals is a decision, not a default.

## 7. Declare your metrics

Edit `goal-engine/config/metrics.yaml`. It ships with four `push` metrics so a fresh
clone works; replace the first three with yours and keep `ops.tokens_spent`, which
is what core reports its own token use to.

A `push` metric is the honest starting point — you send values from whatever
already computes them:

```bash
# A nightly job, with its own bot key.
curl -s https://wingman.example.com/engine/v1/metrics/business.mrr/samples \
  -H "Authorization: Bearer $BILLING_FEED_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"value\": $MRR, \"note\": \"billing nightly\"}"
```

A `sql` metric lets the engine read for itself. Give it a **read-only database
role** and a DSN in its own environment variable:

```yaml
datasources:
  - name: billing
    driver: postgres
    dsnEnv: METRIC_DSN_BILLING
    maxOpenConns: 4

metrics:
  - key: billing.mrr
    description: Monthly recurring revenue
    unit: IDR
    source: sql
    datasource: billing
    query: |
      SELECT COALESCE(SUM(amount), 0) FROM subscriptions WHERE status = 'active'
```

The query text is checked to be a single read and the transaction is opened
read-only, but the database role is what actually enforces it. The DSN itself
never appears in the YAML — only the name of the variable holding it — so the file
stays committable.

Every YAML file either service reads — the engine's `metrics.yaml` and
`policies.yaml`, core's `tools.yaml`, `mcp.yaml` and `http-tools.yaml` — is read
**once, at startup**, and mounted read-only. Changing one needs a restart. That is
deliberate: a limit that can change without a deploy is a limit somebody can change
quietly.

```bash
docker compose restart goal-engine
curl -s localhost:8080/v1/metrics -H "Authorization: Bearer $OP"
```

## 8. Grant core its first tool

`core/config/tools.yaml` ships with `tools: []`, and a tool that is not named there
is **denied** — not passed through, not treated as harmless. A fresh instance can
think, read its own workspace and answer. It cannot act until you say what it may
do.

Start with one, and be honest about its class:

```yaml
tools:
  - name: shell
    description: Run a command in this run's sandbox workspace
    class: read      # true with SANDBOX_BACKEND=docker and SANDBOX_NETWORK=none
```

Three classes, and what each means at call time:

| | |
|---|---|
| `read` | allowed |
| `write` | allowed while somebody is watching the run. **Refused for that run** when nobody is — a goal-engine trigger — and the model is told why, so an unattended run still finishes and reports what it read |
| `spend` | never decided in core. The call is filed with the goal engine's gate, which has the caps, the daily totals and the audit row |

What class the shell deserves depends on your sandbox, not on the tool. With the
docker backend and no network it changes nothing outside a throwaway container and a
workspace directory. Give the sandbox network access, or run
`SANDBOX_BACKEND=local`, and a generated script can reach whatever the host can —
then it is a `write` at least, and on the local backend it is every write core's own
user can perform.

MCP servers go in `core/config/mcp.yaml`, and their tools arrive as
`mcp__<server>__<tool>` — each one still needs its own line in `tools.yaml`, because
a server that gains a capability overnight must not gain it here silently. Start
core with `LOG_LEVEL=debug` to see the qualified names a server offers.

```bash
docker compose restart core
```

## 9. Spend a week in dry run

Leave `TRIGGER_DRY_RUN=true` and let it run. Goals are evaluated, decisions are
recorded, the audit log fills up, and nothing reaches core.

Read what it wanted to do:

```bash
# What did it decide, and how often?
curl -s "localhost:8080/v1/audit?action=goal.trigger_dispatched&limit=50" \
  -H "Authorization: Bearer $OP"
```

You are looking for three failure modes, and all three are goal-definition
problems rather than bugs:

- **A goal that triggers every cooldown.** The target is unreachable, or the
  tolerance is too tight for how noisy the metric is.
- **A goal that never triggers.** The tolerance is too loose, or the metric is
  stale — check for `skipped_invalid_sample`.
- **A metric that moves in steps.** A number updated once a day against an hourly
  monitor will read as behind pace all morning and fine all afternoon. Widen the
  tolerance or lengthen the interval.

When the decisions look like judgements you would have made, set
`TRIGGER_DRY_RUN=false` and restart. That is the moment the assistant becomes
autonomous, and it should be a moment you chose.

The first unattended run is worth reading line by line:

```bash
curl -s "localhost:8081/v1/runs/<id>/steps" -H "Authorization: Bearer $CORE_OP" | jq .data
```

Watch what the brief looked like, which tools it reached for, and whether it stopped
for a reason you would have chosen. `tool_denied` on the first one is normal and
informative — it names exactly what the work needed and you had not granted.

## 10. Turn on spending, narrowly

`goal-engine/config/policies.yaml` ships **empty**, which means every spend request
needs you. That is the correct starting state; do not fix it before you have seen
real requests.

When you do add a policy, add one action type, with a small `autoApproveBelow`:

```yaml
policies:
  - actionType: ads.spend
    enabled: true
    currency: IDR
    autoApproveBelow: 50000      # proceeds without asking
    dailyCap: 500000             # above this: denied, not queued
    hardCap: 250000              # any single request above this: denied
```

The name has to match the `spend.actionType` of the tool in core's `tools.yaml` that
files the request. A name with no policy here asks a human every time, which is this
file's default and not a failure.

Remember the shape of the ladder — caps **refuse**, they do not escalate. Only the
band between `autoApproveBelow` and the caps reaches you. See
[goal-engine.md](goal-engine.md#the-spending-gate) for the full order.

Work the queue:

```bash
curl -s "localhost:8080/v1/approvals?openOnly=true" -H "Authorization: Bearer $OP"

curl -s -XPOST localhost:8080/v1/approvals/<id>/resolve \
  -H "Authorization: Bearer $OP" -H 'Content-Type: application/json' \
  -d '{"resolution":"approved","note":"checked the campaign"}'
```

Unanswered requests expire after `APPROVAL_TTL` (24h). Silence is not consent.

## Operating it

```bash
docker compose logs -f goal-engine core web  # JSON from the services when APP_ENV=production
docker compose ps                            # health
docker compose restart core                  # after a tools.yaml change
```

Every log line carries the `requestId` that produced it, and so does the audit row
for whatever the request changed. That pair is how you get from "something happened
at 18:04" to what and who. A run that came from a trigger carries the goal's id in
its task metadata, so the same walk works across both services. Anything done through
the console arrives with a `web-…` request id, so a row you cannot place came from
`curl` or from an agent.

Watch two numbers rather than all of them: the open approval queue, and
`ops.tokens_spent`. The first is work waiting on you; the second is what the
assistant costs, and it is a metric precisely so a goal can be written against it. The
first is the console's top panel and the count in its status strip; write a goal against
the second and its pace lands on the same screen.

### Stopping it in a hurry

The console has one button for this, top of every screen, and anyone signed in can press
it. `curl` does the same thing:

```bash
curl -s -XPUT localhost:8080/v1/flags/kill-switch \
  -H "Authorization: Bearer $OP" -H 'Content-Type: application/json' \
  -d '{"engaged":true,"reason":"growth agent looping on campaign 42"}'
```

Every trigger halts, every spend is denied, and every agent run stops — core reads
the switch before a run starts and between iterations, fail-closed, so a run in
flight ends at its next iteration with `halted` and a transcript you can read.
Immediate, and one call.

Both services stay up and keep recording. `/readyz` still says ready on both,
because the switch was engaged on purpose and reporting unready would take the
release endpoint out of the load balancer too.

Any credential may engage it. Only a goal-engine operator key can release it:

```bash
curl -s -XPUT localhost:8080/v1/flags/kill-switch \
  -H "Authorization: Bearer $OP" -H 'Content-Type: application/json' \
  -d '{"engaged":false,"reason":"campaign paused upstream, loop fixed"}'
```

A `reason` is required both ways. It is the line the next person reads while working
out why the assistant went quiet. In the console, releasing is the one place a bot key is
visibly not enough: while nobody is holding operator authority there is no release button
at all, only a line saying it needs an operator key and a link to the screen where you
paste one. The whole console stays in its halted state — amber drained out of the
palette — until the release goes through.

If the engine itself is unreachable, core halts anyway: an unreadable switch is an
engaged switch. That is the failure mode you want — the service that cannot ask for
permission does not proceed without it. The console shows the same thing rather than
hiding it: a kill-switch read that fails puts the whole screen in the halted state and
says the engine did not answer.

### Cancelling one run instead of all of them

```bash
curl -s -XPOST localhost:8081/v1/runs/<id>/cancel -H "Authorization: Bearer $CORE_OP"
```

A request, not a kill: the loop reads the flag before its next iteration, so the run
ends with its counters matching what it actually spent. Asking twice is fine. The
console's run page has the same button, and it is your own runs there — cancelling
somebody else's needs `$CORE_OP` and `curl`.

### Rotating a key

Add the new key alongside the old one, restart, move the caller over, then remove
the old one and restart again. Both are valid in between, so nothing has an outage.
Rotating an operator key while an approval is pending is safe — the approval is
recorded against the key's *name*.

For the three cross-service keys, the order matters: add the new secret to the
*receiving* service's list first, restart it, then change the *sending* service's
single value. Doing it the other way round means a window where the engine cannot
dispatch, core cannot ask to spend, or the console cannot read anything.

`WEB_COOKIE_SECRET` is not one of those. Changing it makes every existing cookie
unopenable, which signs everybody out and drops any operator authority being held — the
correct behaviour for a secret whose only job is to make old cookies unreadable, and the
fastest way to end every console session at once.

Rotating a user's password ends every session that account holds, including the one
that changed it. That is the point of the button.

## Backups

Two databases, and they hold different things.

| | |
|---|---|
| `goal-db` | goals, the metric time series, the approval history, the audit trail. Losing it loses your record of what the agent did and who let it. |
| `core-db` | accounts, chats, run transcripts, the token ledger. Losing it loses what the agent actually said and spent. |

```bash
# Nightly, both.
docker compose exec -T goal-db \
  pg_dump -U wingman -d wingman_goals --format=custom \
  > "/var/backups/wingman-goals-$(date +%F).dump"

docker compose exec -T core-db \
  pg_dump -U wingman -d wingman_core --format=custom \
  > "/var/backups/wingman-core-$(date +%F).dump"
```

```bash
# Restore into a fresh volume.
docker compose exec -T goal-db \
  pg_restore -U wingman -d wingman_goals --clean --if-exists < wingman-goals-2026-09-11.dump
```

Back up all four env files and both config directories too — separately,
encrypted, not in git. The dumps are useless without the keys, and the YAML is the
only record of what your limits and grants were.

The sandbox workspace directory is deliberately **not** on this list, and neither is
anything belonging to the console: it has no volume, no database and no state, so a
backup of it would be a backup of `.env.web`, which is in the sentence above. It is scratch
space per person, and anything an agent produced that matters should have left it.

Test a restore before you need one. An untested backup is a hope.

## Upgrades

All three are in this repository, so an upgrade is one pull and the images you
choose to rebuild.

```bash
git pull

cd deploy
docker compose up -d --build goal-engine
docker compose up -d --build core
docker compose up -d --build web
```

Migrations run on boot for both services. They are append-only in practice: no
released migration is edited, so rolling back an image and rolling forward again both
work.

The console has no migrations, no volume and no state, so it is the one container you
can rebuild without thinking about it: it is replaced, it reads its env again, and
whatever it was showing is re-read from the two services. The only visible effect on
anyone using it is a page that needs reloading. Cookies survive — they are sealed with
`WEB_COOKIE_SECRET`, which is in `.env.web` and not in the image.

**Engage the kill switch before upgrading core** if runs are in flight. A run whose
process restarts underneath it is a run nobody finishes — core's sweeper will notice
and mark it `abandoned`, and the task is requeued as a second run, but a task that
was halfway through calling a tool is better stopped deliberately than resumed from
a guess.

The two services are versioned together and deploy independently, which is a
deliberate pair of facts: nothing in either API is allowed to change in a way that
needs both restarted at the same instant. If a change ever does, it is an API change
and says so.

The console is downstream of both and is allowed to be: it reads a fixed list of
routes and renders the envelope. An upgrade that changes a route it reads is the same
API change, and the console is rebuilt from the same pull that changed it.

## Running without Docker

Both binaries have no runtime dependencies beyond a Postgres they can reach — with
one exception, below.

```bash
cd goal-engine
go build -o bin/goal-engine ./cmd/goal-engine
cp .env.example .env && $EDITOR .env
./bin/goal-engine
```

```bash
cd core
go build -o bin/core ./cmd/core
cp .env.example .env && $EDITOR .env
read -rs PW && printf '%s' "$PW" | ./bin/core createuser -email you@example.com -name 'Your Name'
./bin/core
```

The exception is core's sandbox. `SANDBOX_BACKEND=docker` needs a `docker` binary on
`PATH` and a daemon it can reach; without one, set `SANDBOX_BACKEND=local` and read
what `internal/sandbox/local.go` says about itself — it runs the generated script on
this host as the user core runs as. It is not isolation and does not pretend to be.
It exists so a laptop can drive a run end to end. Do not run it in front of other
people, and never inside a container image that holds your API keys.

The console needs Node 22 or newer, and needs it at build time as well as run time:

```bash
cd web
cp .env.example .env.local && $EDITOR .env.local
npm ci
npm run build

# Next's standalone output: the traced server plus the assets it serves. The second
# copy is not optional — standalone deliberately omits .next/static, and without it
# every page loads with no stylesheet and no font.
cp -r .next/static .next/standalone/.next/static

PORT=3000 HOSTNAME=127.0.0.1 node .next/standalone/server.js
```

The build downloads Archivo and IBM Plex Mono, so it needs network once. The running
console does not: the fonts are served from its own origin afterwards, which is the
point — a dashboard that renders differently when the network is down is a dashboard
lying about the network being down.

`npm run start` also works and is what you want while changing the thing. It serves
from `.next/` in place, needs the dev tree present, and re-reads nothing you have not
rebuilt. `.next/standalone/` is what to deploy: it holds no devDependencies, and it is
what the Dockerfile ships.

`.env.local` is read by `next build` and `next start`, but **not** by
`node .next/standalone/server.js` — that is a plain Node process and knows nothing
about Next's env files. Either export the variables into its environment or point
systemd's `EnvironmentFile` at the same file. A console started this way with an empty
environment does not crash on boot: it starts, then answers 500 on every screen, with
`wingman web is not configured: …` in its log naming each missing variable. That
message is deliberately not shown in the browser — see the next section.

A systemd unit, if you go this way:

```ini
[Unit]
Description=Wingman goal engine
After=network-online.target postgresql.service

[Service]
Type=simple
User=wingman
WorkingDirectory=/opt/wingman/goal-engine
EnvironmentFile=/opt/wingman/goal-engine/.env
ExecStart=/opt/wingman/goal-engine/bin/goal-engine
Restart=on-failure
RestartSec=5s
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/opt/wingman/goal-engine

[Install]
WantedBy=multi-user.target
```

The same unit works for core with the paths changed, plus
`ReadWritePaths=/srv/wingman/workspaces` and, on the docker backend,
`SupplementaryGroups=docker`. `NoNewPrivileges=true` and `ProtectSystem=strict` are
worth keeping on both: neither service has any reason to write outside its own
directory.

For the console it is the same unit with
`ExecStart=/usr/bin/node /opt/wingman/web/.next/standalone/server.js`,
`EnvironmentFile=/opt/wingman/web/.env.local` and no `ReadWritePaths` at all — it
writes nothing, so `ProtectSystem=strict` plus `ReadOnlyPaths=/opt/wingman/web` costs
it nothing. Add `After=goal-engine.service core.service` for tidiness rather than
correctness: the console does not need either service to be up in order to start, and
a screen that says a read failed is more use than a console that will not boot because
something else is down.

`WorkingDirectory` matters — `MIGRATIONS_DIR` and every config path is relative by
default. Both Go processes handle `SIGTERM` by draining in-flight requests and running
workers for `SHUTDOWN_GRACE`, so `systemctl restart` does not cut a decision or a
run in half. The console has nothing to drain: no queue, no worker, no state. Killing
it interrupts whichever reads were in flight, and the browser retries on the next
poll.

## Troubleshooting

**`POSTGRES_PASSWORD` variable is not set** — compose refusing to start, on purpose.
There is no default password anywhere in this repo. Same for
`CORE_POSTGRES_PASSWORD`.

**Either service restarts in a loop** — `docker compose logs goal-engine` or
`docker compose logs core`. Both validate their whole configuration at startup and
refuse to boot half-configured, so the log line names the field. Common causes: a
key shorter than 24 characters, a secret containing a colon, an unparseable duration
(`MONITOR_INTERVAL=1hr`), a `metricKey` in a goal that no longer exists in
`metrics.yaml`, a `DEFAULT_MODEL` left empty, or a `CORE_UNATTENDED_OWNER` naming an
account that was never created.

**Core panics at startup about a key shaped like a session token** — a value in
`CORE_API_KEYS` or `CORE_BOT_KEYS` looks like a `wgm_` session token. Core tells the
three principals apart by the credential's shape, so such a key would never be
matched and would look permanently revoked instead. Generate another.

**`/readyz` says `metrics: 0`** — the `../goal-engine/config` volume did not mount,
or `METRICS_CONFIG_PATH` points somewhere else. The service starts fine with no
metrics; goals just cannot be created.

**Everything returns 401** — you are sending a key that is in neither list. Restart
after editing an env file; the lists are read at startup.

**A goal never triggers** — check `GET /v1/goals/:id` for its last decision.
`skipped_invalid_sample` means no reading or one older than
`METRIC_MAX_SAMPLE_AGE` (26h): the feed is dead, and the engine is refusing to judge
a goal on a stale number rather than reading it as on-track.
`skipped_not_started` means `periodStart` is in the future.

**A goal triggers but core sees nothing** — `TRIGGER_DRY_RUN` is probably still
`true`, which is the intended default. If it is `false`, look for
`goal.trigger_failed` in the audit log: it carries the HTTP status core returned.
`401` there means `WINGMAN_CORE_API_KEY` is not in core's `CORE_BOT_KEYS`. The
engine's image has no shell to debug from, but core's does:

```bash
docker compose exec core sh -c 'wget -qO- http://goal-engine:8080/healthz'
```

**A run stops immediately with `halted`** — the kill switch is engaged, or core
cannot read it. Check `GET /v1/flags/kill-switch` on the engine, and core's logs for
a failed read. Unreadable means engaged; that is the design, not a bug.

**Every run stops with `tool_denied`** — `core/config/tools.yaml` grants nothing,
which is what it ships doing. The stop reason names the tool the work needed. See
step 8.

**A tool call fails and the transcript mentions no such image** — `SANDBOX_IMAGE` is
still `wingman/sandbox:latest`, which nothing publishes. Point it at an image you
trust that has the toolchain the work needs.

**A tool call succeeds but the workspace is empty** — `SANDBOX_WORKSPACE_ROOT`
inside core's container is not the same path as the host directory it is mounted
from. The daemon resolves that bind mount against the *host's* filesystem, so the
two have to be identical. Set `CORE_WORKSPACE_DIR` in `deploy/.env` and let compose
do both sides.

**A tool call fails with a permission error on the docker socket** —
`CORE_DOCKER_GID` does not match the host. `stat -c '%g' /var/run/docker.sock`.

**A write tool is refused only on unattended runs** — working as designed. A `write`
grant applies while somebody is watching, because a person in a chat can undo a
wrong message in seconds and a triggered run at 3am cannot. The model is told why,
so the run still finishes and reports what it read.

**`/v1/me/ledger` says `isReadable: false`** — the token ledger could not be read.
That is not "nothing spent", it is "no answer", and it is the condition under which
a run stops with `budget_unreadable` rather than guessing. Check `core-db`.

**Core says it is listening but a chat platform never delivers anything** — the
`core listening` line lists what it built a channel for (`channels`), which means the
credential was well formed, not that the platform accepted it. If the platform is on that
list and still silent, look for the adapter's own error in the log: each one asks its
platform who this bot is before it starts reading, which is the call a rejected token
fails loudly on — long polling would retry a rejected read for ever and look exactly like
a quiet platform. That one channel stops; the others and every HTTP route carry on.

**The bot answers a group message with "direct messages only"** — `CHANNEL_ALLOW_GROUPS`
is `false`, which is the default. In a shared room the linked person's budget is spendable
by anybody who can type, so this is a decision rather than an oversight. Turning it on
warns at every startup, and even on, core acts only on a message that addresses the bot.

**A link code is always refused** — the whole message has to be the code and nothing else;
a code with "hi" in front of it is not one. Unknown, expired and already spent are one
answer on purpose, so the sender cannot tell which they hit — mint a fresh code and send
it on its own. Minting also invalidates the previous one, so the older of two codes is
dead.

**A chat that used to work now asks to be linked** — the connection was revoked, from
`DELETE /v1/channels/:id`. `GET /v1/channels` shows revoked rows with `isLive: false`. Only
the account that released it can pick it up again; a code redeemed from any other account
is told an operator has to clear it.

**Notifications are on and nothing arrives** — the engine's log is the place to look, and
there are three distinct answers there. No line at all means `NOTIFY_ENABLED` is still
`false`, or the moment never happened: only a dispatched trigger and an approval that came
back `pending` produce one, so `auto_approved` and `denied` are silent by design.
`could not notify` with a `401` means `WINGMAN_CORE_API_KEY` is not in core's
`CORE_BOT_KEYS` — the same credential the trigger bridge uses. And `notification sent`
while your phone stays quiet means core reached nobody: the account
`CORE_UNATTENDED_OWNER` names has no live chat identity, so do the linking step above,
signed in as **that** account. Core says so in its own answer — `recipients: 0` and
"Nobody was notified" — and a revoked identity counts as none.

**A notification arrives with no amount in it** — working as designed, and not negotiable
by configuration. A chat message lives on somebody else's servers, and a message complete
enough to approve from is a message people approve from at a glance. The link is the
feature; if there is no link either, `WEB_BASE_URL` is unset.

**Every console screen is a blank server error** — `docker compose logs web`. The
console validates its configuration on the first request that needs it, not at boot,
so a missing variable looks like a container that started and then serves 500s. The
log line is `wingman web is not configured:` followed by every problem at once. It
names variables, never values, and it is deliberately not rendered in the browser:
Next replaces a server-side error with an opaque page, and this one would otherwise
tell an anonymous visitor which credentials the box is missing. Common causes: a
`CHANGE_ME` left in place, or a `WEB_COOKIE_SECRET` that is not 32 bytes.

**The console renders perfectly and no button works** — sign-in, the kill switch,
every resolve button dead, while the screen looks entirely normal. That is a
Content-Security-Policy problem, and almost always a reverse proxy setting its own
`Content-Security-Policy` header over the console's. The console's policy carries a
per-request nonce because a streamed React tree ships its payload in inline `<script>`
tags; a static `script-src 'self'` from a proxy blocks them, the client never receives
the payload, and React fails hydration silently. Check the response headers for two
CSPs or one without a `nonce-`. Do not let your proxy write this header for this
origin; `web/proxy.ts` explains what it has to contain.

**Sign-in succeeds and lands straight back on `/signin`, for ever** — you are reaching
the console over plain HTTP on something that is not `localhost`. The session cookie
is `Secure` in production, browsers only exempt `localhost` and `127.0.0.1` from that,
so the browser accepts the redirect and then sends no cookie. There is nothing to fix
in the console: put TLS in front of it, or tunnel to it —
`ssh -N -L 3000:127.0.0.1:3000 you@box` and use `http://localhost:3000`.

**One panel says the service could not be reached, the rest are fine** — that is the
design: each read is wrapped, and a failed one degrades to a line under the panel
heading instead of a 500 for the whole deck. `The service did not answer within
15000ms.` is `WEB_UPSTREAM_TIMEOUT_MS` elapsing; `The service could not be reached.`
is a wrong `GOAL_ENGINE_BASE_URL` or `CORE_BASE_URL`, or the service being down. The
console is on a docker network under compose, so `localhost` in either URL means the
console's own container.

**The whole console is in its halted state and the engine looks up** — the kill switch
read failed, and unreadable means engaged here too. The panel error says which read
failed while the screen says engaged; both are true at once. Same rule as the engine
and core.

**The kill switch offers no Release button** — no operator authority is held. Engaging
is deliberately open to anyone signed in; releasing is not. Paste an operator key on
`/authority`. It is held for `WEB_OPERATOR_TTL_MINUTES` (30 by default), does not
renew, and the countdown is on the strip.

**A pasted operator key is refused** — the console checks it before sealing it, with a
side-effect-free probe. "A bot key" means the key is real but is in
`GOAL_ENGINE_BOT_KEYS`; holding it would buy nothing, so it is not held. "Not
recognised" means it is in neither list, or the engine was restarted after the env
file changed.

**Every request is rate-limited from one IP** — `TRUSTED_PROXIES` is empty behind a
proxy, so every caller shares the proxy's bucket. Set it, in whichever service file
is behind the proxy. Note that the console is itself one caller as far as core is
concerned: everybody's sign-in attempts share its address, which is a reason to keep
it off the internet rather than a reason to set `TRUSTED_PROXIES` on it — it has no
such setting, because it makes no rate-limiting decision.

**A spend was denied and you expected a question** — that is the ladder working.
Caps deny; they do not queue. Raise the cap in `policies.yaml` and restart, which is
a decision you make deliberately rather than one you tap at midnight.
