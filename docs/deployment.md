# Deployment

Self-hosting Wingman on one VPS, from an empty box to the first trigger. Roughly
30 minutes, plus a week of watching before you let it act.

- [What you need](#what-you-need)
- [1. The box](#1-the-box)
- [2. Clone and provision core](#2-clone-and-provision-core)
- [3. Configure](#3-configure)
- [4. Start](#4-start)
- [5. Verify](#5-verify)
- [6. Put a reverse proxy in front](#6-put-a-reverse-proxy-in-front)
- [7. Declare your metrics](#7-declare-your-metrics)
- [8. Spend a week in dry run](#8-spend-a-week-in-dry-run)
- [9. Turn on spending, narrowly](#9-turn-on-spending-narrowly)
- [Operating it](#operating-it)
- [Backups](#backups)
- [Upgrades](#upgrades)
- [Running without Docker](#running-without-docker)
- [Troubleshooting](#troubleshooting)

## What you need

| | |
|---|---|
| A VPS | 2 vCPU / 4 GB is comfortable for the goal engine and its database. Core's sandbox wants more — size for core, not for this. |
| Docker | Engine 24+ with the Compose plugin. |
| A domain | Only if you want the API reachable from outside the box. It does not have to be. |
| An LLM API key | For core. The goal engine calls no model. |

The goal engine itself is a single static binary and a Postgres database. It is
not the part that will strain the machine.

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
This is why `deploy/docker-compose.yml` publishes to `127.0.0.1:8080` instead. If
you change that binding, you have opened a port, and ufw will not tell you.

## 2. Clone and provision core

```bash
git clone https://github.com/ribdsp/wingman.git
cd wingman

./scripts/fork-core.sh --repo <you>/wingman-core --ref v1.0.0
```

The script clones your fork of Rakazo into `core/` and records what it cloned in
`core/.wingman-core-ref`. Pin a tag. Tracking `main` means an upstream commit can
change your agent runtime between two `docker compose up`s, and the goal engine
will happily keep dispatching into it.

You do not need a fork on day one — `--repo elie222/rakazo` works and the script
will warn you that you are cloning upstream. You will want your own fork before
you rebrand anything: see [fork-and-rebrand.md](fork-and-rebrand.md).

Follow core's own README to configure and start it. It has its own database, its
own environment, and its own compose file; this repo does not manage them.

## 3. Configure

```bash
cd deploy
cp ../goal-engine/.env.example .env
chmod 600 .env
$EDITOR .env
```

Five values decide whether this works. The rest have defaults that are fine.

```bash
POSTGRES_PASSWORD=            # openssl rand -hex 24
GOAL_ENGINE_API_KEYS=ops:     # openssl rand -hex 24 — your operator key
GOAL_ENGINE_BOT_KEYS=wingman-core:   # openssl rand -hex 24 — one per agent/feed
WINGMAN_CORE_BASE_URL=http://core:3000
WINGMAN_CORE_API_KEY=         # issued by core
```

```bash
openssl rand -hex 24   # run this four times; do not reuse one value twice
```

Keys are `name:secret`. The name is what the audit log records, so name them after
who they are — an audit trail that can only say "some valid key" does not answer
"who moved that target". A secret is at least 24 characters and must not contain a
colon.

Which list a key is in is the **only** thing that decides what it can do:

- `GOAL_ENGINE_API_KEYS` — you. Can resolve approvals, edit goal targets, release
  the kill switch.
- `GOAL_ENGINE_BOT_KEYS` — agents and cron jobs. Can state goals, read, report
  metric values, ask to spend. Cannot do anything in the list above.

Give every metric feed its own bot key rather than reusing the one an agent thinks
with. A reported number is exactly as trustworthy as the credential that sent it.

Leave `TRIGGER_DRY_RUN=true`. Step 8 is about why.

### Joining the two services

```bash
docker network create wingman-net
```

Then add to **both** compose files:

```yaml
networks:
  default:
    name: wingman-net
    external: true
```

and set `WINGMAN_CORE_BASE_URL=http://core:3000` in `deploy/.env`, matching core's
service name and port. Nothing between the two services needs to be published to
the host.

## 4. Start

```bash
docker compose up -d --build
docker compose ps
```

`goal-db` becomes healthy first; `goal-engine` waits for it, because it applies
migrations on boot (`AUTO_MIGRATE=true`) and cannot do that against a database
that is still initialising.

## 5. Verify

```bash
curl -s localhost:8080/readyz
# {"status":"ready","killSwitchEngaged":false,"metrics":3}
```

`metrics: 3` is the three push metrics that ship in `config/metrics.yaml`. If it
says `0`, the config volume did not mount.

```bash
export OP=<your operator secret>

# Auth works, and the enums a dashboard needs.
curl -s localhost:8080/v1/reference -H "Authorization: Bearer $OP" | head -c 400

# An unauthenticated call is refused.
curl -s -o /dev/null -w '%{http_code}\n' localhost:8080/v1/goals    # 401
```

Now walk one goal all the way through:

```bash
# State it.
curl -s localhost:8080/v1/goals -H "Authorization: Bearer $OP" \
  -H 'Content-Type: application/json' \
  -d '{"product":"acme","title":"MRR to 50M by end of Q4",
       "sourceText":"kejar MRR 50 juta sebelum akhir kuartal",
       "metricKey":"business.mrr","targetValue":50000000,
       "periodEnd":"2026-12-31T23:59:59+07:00"}'

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

Only if you need the API from outside the box. If your agents run on the same
host, you do not.

```nginx
server {
    listen 443 ssl http2;
    server_name wingman.example.com;

    ssl_certificate     /etc/letsencrypt/live/wingman.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/wingman.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

Then, in `deploy/.env`:

```bash
TRUSTED_PROXIES=127.0.0.1
```

This one matters in both directions. Left empty behind a proxy, every caller
shares one rate-limit bucket, because they all appear to come from the proxy. Set
too broadly, a client can send its own `X-Forwarded-For` and pick its bucket.
List the proxy, and nothing else.

The service speaks plain HTTP on purpose — TLS belongs to the thing that already
has your certificates and renews them.

## 7. Declare your metrics

Edit `goal-engine/config/metrics.yaml`. It ships with three `push` metrics so a
fresh clone works; replace them with yours.

A `push` metric is the honest starting point — you send values from whatever
already computes them:

```bash
# A nightly job, with its own bot key.
curl -s https://wingman.example.com/v1/metrics/business.mrr/samples \
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

Both files are read **once, at startup**. Changing them needs
`docker compose restart goal-engine`. That is deliberate: a limit that can change
without a deploy is a limit somebody can change quietly.

```bash
docker compose restart goal-engine
curl -s localhost:8080/v1/metrics -H "Authorization: Bearer $OP"
```

## 8. Spend a week in dry run

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

## 9. Turn on spending, narrowly

`config/policies.yaml` ships **empty**, which means every spend request needs you.
That is the correct starting state; do not fix it before you have seen real
requests.

When you do add a policy, add one action type, with a small
`autoApproveBelow`:

```yaml
policies:
  - actionType: ads.spend
    enabled: true
    currency: IDR
    autoApproveBelow: 50000      # proceeds without asking
    dailyCap: 500000             # above this: denied, not queued
    hardCap: 250000              # any single request above this: denied
```

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
docker compose logs -f goal-engine          # JSON when APP_ENV=production
docker compose ps                           # health
docker compose restart goal-engine          # after a config change
```

Every log line carries the `requestId` that produced it, and so does the audit row
for whatever the request changed. That pair is how you get from "something
happened at 18:04" to what and who.

### Stopping it in a hurry

```bash
curl -s -XPUT localhost:8080/v1/flags/kill-switch \
  -H "Authorization: Bearer $OP" -H 'Content-Type: application/json' \
  -d '{"engaged":true,"reason":"growth agent looping on campaign 42"}'
```

Every trigger halts and every spend is denied, immediately. The service stays up
and keeps recording — `/readyz` still says ready, because the switch was engaged
on purpose and reporting unready would take the release endpoint out of the load
balancer too.

Any credential may engage it. Only an operator key can release it:

```bash
curl -s -XPUT localhost:8080/v1/flags/kill-switch \
  -H "Authorization: Bearer $OP" -H 'Content-Type: application/json' \
  -d '{"engaged":false,"reason":"campaign paused upstream, loop fixed"}'
```

A `reason` is required both ways. It is the line the next person reads while
working out why the engine went quiet.

### Rotating a key

Add the new key alongside the old one, restart, move the caller over, then remove
the old one and restart again. Both are valid in between, so nothing has an
outage. Rotating an operator key while an approval is pending is safe — the
approval is recorded against the key's *name*.

## Backups

The goal engine's database holds the audit trail, the approval history, and the
metric time series. Losing it loses your record of what the agent did.

```bash
# Nightly, into a file the postgres container never sees again.
docker compose exec -T goal-db \
  pg_dump -U wingman -d wingman_goals --format=custom \
  > "/var/backups/wingman-$(date +%F).dump"
```

```bash
# Restore into a fresh volume.
docker compose exec -T goal-db \
  pg_restore -U wingman -d wingman_goals --clean --if-exists < wingman-2026-09-11.dump
```

Back up `deploy/.env` and `goal-engine/config/*.yaml` too — separately, encrypted,
not in git. The dump is useless without the keys, and the YAML is the only record
of what your limits were.

Test a restore before you need one. An untested backup is a hope.

## Upgrades

### The goal engine

```bash
git pull
cd deploy && docker compose up -d --build goal-engine
```

Migrations run on boot. They are append-only in practice: no released migration is
edited, so rolling back the image and rolling forward again both work.

### Core

Core is your fork, so upgrading it is a merge:

```bash
cd core
git fetch upstream
git log --oneline HEAD..upstream/main      # read this before merging
git merge upstream/v1.1.0
```

Pin a tag, read the diff, and expect the merge to be routine — the whole reason
the goal engine is a separate service is that none of this project's own features
live in files upstream touches. Details in
[fork-and-rebrand.md](fork-and-rebrand.md).

Engage the kill switch before a core upgrade if agents are mid-task. A task
dispatched into a service that restarts underneath it is a task nobody finishes.

## Running without Docker

The binary has no runtime dependencies beyond a Postgres it can reach.

```bash
cd goal-engine
make build                    # or: go build -o bin/goal-engine ./cmd/goal-engine
cp .env.example .env && $EDITOR .env
./bin/goal-engine
```

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

`WorkingDirectory` matters: `MIGRATIONS_DIR` and the two config paths are relative
by default. The process handles `SIGTERM` by draining in-flight requests for
`SHUTDOWN_GRACE`, so `systemctl restart` does not cut a decision in half.

## Troubleshooting

**`POSTGRES_PASSWORD` variable is not set** — compose refusing to start, on
purpose. There is no default password anywhere in this repo.

**Goal engine restarts in a loop** — `docker compose logs goal-engine`. The
process validates its whole configuration at startup and refuses to boot
half-configured, so the log line names the field. Common causes: a key shorter
than 24 characters, a secret containing a colon, an unparseable duration
(`MONITOR_INTERVAL=1hr`), or a `metricKey` in a goal that no longer exists in
`metrics.yaml`.

**`/readyz` says `metrics: 0`** — the `../goal-engine/config` volume did not
mount, or `METRICS_CONFIG_PATH` points somewhere else. The service starts fine
with no metrics; goals just cannot be created.

**Everything returns 401** — you are sending a key that is in neither list.
Restart after editing `.env`; the lists are read at startup.

**A goal never triggers** — check `GET /v1/goals/:id` for its last decision.
`skipped_invalid_sample` means no reading or one older than
`METRIC_MAX_SAMPLE_AGE` (26h): the feed is dead, and the engine is refusing to
judge a goal on a stale number rather than reading it as on-track.
`skipped_not_started` means `periodStart` is in the future.

**A goal triggers but core sees nothing** — `TRIGGER_DRY_RUN` is probably still
`true`, which is the intended default. If it is `false`, look for
`goal.trigger_failed` in the audit log: it carries the HTTP status core returned.
Check `WINGMAN_CORE_BASE_URL` resolves from inside the container
(`docker compose exec goal-engine ...` will not help — the image has no shell;
check from `goal-db` instead, or read the logs).

**Every request is rate-limited from one IP** — `TRUSTED_PROXIES` is empty behind
a proxy, so every caller shares the proxy's bucket. Set it to the proxy's address.

**A spend was denied and you expected a question** — that is the ladder working.
Caps deny; they do not queue. Raise the cap in `policies.yaml` and restart, which
is a decision you make deliberately rather than one you tap at midnight.
