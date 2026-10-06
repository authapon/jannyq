# Deploying and operating jannyq

This guide is for running jannyq for other people: what is exposed, how to keep it safe, how to watch it, back it up and
upgrade it. Everything here is about the Docker Compose setup of this repository; a plain-binary variant with systemd is at the end.

> **What was and was not verified.** The Go code in this guide (metrics, backups, restore, retention, readiness) is covered by tests and
> was run as a real binary. The Docker images, `docker-compose` files and the systemd units were **not** built or started in the
> environment where they were written (it has no Docker daemon or systemd). Start with a test deployment, and run
> `docker compose config`, `docker compose build` and a restore drill (below) before you rely on them.

## 1. Know what you run

```
 internet ──► Caddy (HTTPS) ──► jannyq ──► model (Ollama / OpenAI-compatible API)
                                  │   └──► SearXNG, web pages (web_fetch, SSRF-guarded)
                                  │
                                  └──(internal network, token)──► sandbox  ← runs the model's shell commands, reads PDFs
                                  └──► data volume: chats, files, knowledge.db, web_secret
```

| Component | Holds | Can reach |
|---|---|---|
| **jannyq** | channel tokens, API keys, all conversations, the web session key | the model API, SearXNG, the internet (web_fetch), the sandbox |
| **sandbox** | nothing of the bot's (a token to accept calls), the workspaces of the chats | nothing outside its internal network, unless you add egress |
| **Caddy** | the TLS certificate | jannyq only |

The design rule: **whatever the model or a user can influence runs in the sandbox, which has no secrets.** The bot itself never runs
commands, and PDFs, which are untrusted input to a large parser, are read in the sandbox as well (`--pdf-engine=auto` with `--sandbox-url` set).

## 2. Before you expose it: checklist

- [ ] **Who may talk to it?** `JANNYQ_ALLOWED_USERS` (empty = everyone), `JANNYQ_RATE_LIMIT`, and for the web chat `JANNYQ_WEB_ACCESS_CODE`.
  Model time is the expensive resource; an open bot on the internet will be used by strangers.
- [ ] **Commands.** `JANNYQ_RUN_COMMAND=sandbox` only. Never `host` for anything but a private test. Keep `JANNYQ_SANDBOX_NETWORK=off`
  (the default) unless you have read `docker-compose.egress.yml`.
- [ ] **Secrets.** Generate `JANNYQ_SANDBOX_TOKEN` with `openssl rand -hex 32`. Prefer files over environment variables
  (`JANNYQ_TELEGRAM_TOKEN_FILE=/run/secrets/...`): environment variables show up in `docker inspect`. Never commit `.env`.
- [ ] **HTTPS.** Webhook channels (LINE, Messenger, WhatsApp) and the web chat go through `docker-compose.public.yml`; only Caddy publishes ports.
  The bot's own port stays on the private `edge` network and trusts `X-Forwarded-*` from there only (`JANNYQ_TRUSTED_PROXIES`).
- [ ] **Stronger isolation for the sandbox** if untrusted people can run commands: install [gVisor](https://gvisor.dev) and uncomment
  `runtime: runsc` in `docker-compose.yml`, or put the sandbox on a separate machine (`JANNYQ_SANDBOX_URL` can point anywhere; use a private network
  or a TLS-terminating proxy, because the token travels with every call). A container shares the host kernel; a kernel bug can cross it.
- [ ] **Disk quotas.** `/work` of the sandbox and the data volume grow with the number of chats. Put them on volumes with a size limit
  (`--attach-chat-mb`, the sandbox's `--quota-mb` and idle clean-up bound each chat, not their number).
- [ ] **Privacy.** Decide how long conversations may be kept (`JANNYQ_RETENTION_DAYS`) and tell your users what is stored (section 7).
- [ ] **Updates.** Rebuild images regularly (`docker compose build --pull`); CI runs `govulncheck` against the dependencies and the Go standard library.

Not included on purpose: a container-per-chat Docker backend for the sandbox. It would need the Docker socket inside the bot,
which gives whoever breaks the bot control of the host, the opposite of what the sandbox is for. The per-chat Unix users plus gVisor
(or a separate machine) give strong isolation without that.

## 3. Install

```sh
git clone https://github.com/authapon/jannyq && cd jannyq
cp .env.example .env            # set the model, JANNYQ_SANDBOX_TOKEN and at least one channel
docker compose config >/dev/null    # catches typos before anything starts
docker compose up -d --build
docker compose logs -f jannyq
```

For public access: set `JANNYQ_DOMAIN` (DNS must point at the server, ports 80/443 open) and add the override:

```sh
docker compose -f docker-compose.yml -f docker-compose.public.yml up -d --build
```

Health: `/healthz` (the process is up) and `/readyz` (the data directory is writable and the knowledge database answers; it does
not depend on the model or on any platform, so an outage elsewhere does not restart the bot). Both are exempt from the rate limit.

## 4. Monitoring

Set `JANNYQ_METRICS_LISTEN=:9100` (inside Docker) and a `JANNYQ_METRICS_TOKEN`. The metrics are served on **their own port**, never on
the public web server, and are not published by the compose files; scrape them from another container on the same network.
Without a token the bot warns if the address is not loopback.

```yaml
# prometheus.yml
scrape_configs:
  - job_name: jannyq
    authorization: { credentials_file: /etc/prometheus/jannyq-token }   # the token, in a file
    static_configs: [{ targets: ["jannyq:9100"] }]
```

| Metric | Meaning |
|---|---|
| `jannyq_messages_received_total{channel,addressed}` | messages from users (group chatter included, `addressed="false"`) |
| `jannyq_messages_rejected_total{channel,reason}` | `not_allowed`, `rate_limited`, `busy` |
| `jannyq_replies_total{channel,result}` | `ok`, `empty`, `error` |
| `jannyq_request_duration_seconds{channel}` | histogram: time to answer, tools included |
| `jannyq_llm_requests_total{result}`, `jannyq_llm_request_duration_seconds`, `jannyq_llm_tokens_total{type}` | the model |
| `jannyq_tool_calls_total{tool,result}` | tools (`unknown` for names the model made up) |
| `jannyq_compactions_total{result}` | conversation summaries |
| `jannyq_attachments_total{kind,result}` | files: `read`, `failed`, `skipped` |
| `jannyq_knowledge_files{status}`, `jannyq_knowledge_passages{embedded}`, `jannyq_knowledge_indexing_pending_files`, `jannyq_knowledge_last_scan_timestamp_seconds`, `jannyq_knowledge_scans_total`, `jannyq_knowledge_changes_total{change}`, `jannyq_knowledge_embed_failures_total` | the knowledge base |
| `jannyq_open_sessions` | chat databases open |
| `jannyq_http_requests_total{class}` | `2xx`…`5xx` of the web server |
| `jannyq_backups_total{result}`, `jannyq_last_backup_timestamp_seconds`, `jannyq_retention_deleted_total{what}` | housekeeping |
| `go_*`, `process_start_time_seconds`, `jannyq_build_info{version}` | process |

Label values are bounded (a metric stops adding series at 500, and reports what it dropped), so nothing a user types can grow the registry.

Alerts worth having:

```yaml
groups:
  - name: jannyq
    rules:
      - alert: JannyqNoBackup
        expr: time() - jannyq_last_backup_timestamp_seconds > 2 * 86400   # also fires when there never was one (0)
        for: 10m
      - alert: JannyqBackupFailing
        expr: increase(jannyq_backups_total{result="error"}[6h]) > 0
      - alert: JannyqModelErrors
        expr: rate(jannyq_llm_requests_total{result="error"}[10m]) / rate(jannyq_llm_requests_total[10m]) > 0.3
        for: 10m
      - alert: JannyqSlowAnswers
        expr: histogram_quantile(0.9, rate(jannyq_request_duration_seconds_bucket[15m])) > 90
        for: 15m
      - alert: JannyqKnowledgeStuck
        expr: time() - jannyq_knowledge_last_scan_timestamp_seconds > 3600 and jannyq_knowledge_last_scan_timestamp_seconds > 0
      - alert: JannyqDown
        expr: up{job="jannyq"} == 0
        for: 5m
```

Logs go to stderr (`--log-json` for machines, `--log-level debug` for detail). They name chats by platform id and sometimes file names, never contain message
text, and redact bot tokens. Let Docker rotate them (`logging: { driver: json-file, options: { max-size: 10m, max-file: "5" } }`).

## 5. Backups

`JANNYQ_BACKUP_DIR` turns on automatic backups: one every `JANNYQ_BACKUP_INTERVAL` (24 h; the first a minute after start, or when the
newest is that old), the newest `JANNYQ_BACKUP_KEEP` (7) kept. The compose file stores them in the `jannyq-backups` volume.

A backup is one `jannyq-backup-<UTC time>.tar.gz` (mode 0600) with: every chat database, the knowledge database, the files users sent
(`JANNYQ_BACKUP_FILES=false` leaves them out), the web session key and the command audit log, plus a manifest with a checksum of
each file. Databases are copied with SQLite's `VACUUM INTO`, a consistent snapshot taken while the bot keeps running; `-wal` and `-shm`
files are not needed. It is written under a temporary name and renamed when complete, so a crash never leaves a half backup that looks whole.
**It contains private conversations and the key that signs web sessions: protect it like the data itself.**

**Same disk is not a backup.** Copy the folder off the machine (the point of a backup is surviving the loss of the disk), for example
`rclone sync /var/lib/docker/volumes/<project>_jannyq-backups/_data remote:jannyq-backups` from a daily cron job, encrypted at the destination.

By hand:

```sh
docker compose exec jannyq jannyq backup --out /backups          # make one now
docker compose exec jannyq jannyq verify /backups/jannyq-backup-….tar.gz
```

`verify` unpacks into a scratch directory, checks every checksum and runs `PRAGMA integrity_check` on every database.

**Restore** (stop the bot first; restoring under a running bot would be overwritten by it):

```sh
docker compose stop jannyq
docker compose run --rm --no-deps jannyq restore --from /backups/jannyq-backup-….tar.gz            # empty data directory
docker compose run --rm --no-deps jannyq restore --from /backups/….tar.gz --force                   # replace existing data
docker compose start jannyq
```

Restore unpacks into a staging directory first and checks the archive completely (paths, file types, sizes, checksums, database
integrity) before touching your data. Hostile or damaged archives (`../` paths, links, devices, missing or wrong checksums, truncation)
are refused and leave nothing behind. With `--force` the existing chats, knowledge database, key and logs are **moved aside**
(`sessions.before-restore-<time>`, …), not deleted. The sandbox workspaces (the files commands created) are not part of the backup.

Restore only backups you made yourself: a database from an untrusted source can carry triggers or views that run when the bot uses it.

**Do a restore drill** once, on a spare machine, before you need it, and again after each major upgrade.

## 6. Upgrades

1. Make a backup (`jannyq backup`) and copy it away.
2. `git pull && docker compose build --pull && docker compose up -d`.
3. Watch `docker compose logs -f jannyq` and `/readyz`.

Database schemas migrate forward automatically on first use (`PRAGMA user_version`); there is **no downgrade**. To go back to an older
version, restore the backup made in step 1 together with the older image.

## 7. What is stored, and for how long

| Data | Where | Removed by |
|---|---|---|
| Messages, summaries, who said what and when | `<data>/sessions/<channel>/<chat>/session.db` | `/reset`, `JANNYQ_RETENTION_DAYS`, deleting the folder |
| Files users sent (resized pictures, PDFs, text) | `…/<chat>/files/` | the same; oldest first above `--attach-chat-mb` |
| Copies for `run_command` | the sandbox workspace `inbox/` | `/reset`, the sandbox's idle clean-up (7 days) |
| Knowledge base | `<data>/knowledge.db`, from the folder you mount | the folder is the source of truth: delete the file and the index follows |
| Web session key | `<data>/web_secret` | rotating it signs everybody out |
| Command log (who ran what) | `<data>/audit/commands.jsonl` | rotates at 10 MB; delete as your policy says |
| Backups | `JANNYQ_BACKUP_DIR` | `JANNYQ_BACKUP_KEEP`, and your off-site copies |

`JANNYQ_RETENTION_DAYS=N` deletes, every hour, each chat whose database has not been written to for N days (messages **and** files); a
chat being served is never touched, and a person who comes back starts afresh. Backups still hold what was deleted until they rotate out.
To erase one person: find their chat folder (`sessions/<channel>/<id>-<hash>/`), delete it, delete their sandbox workspace (`/reset` does it
for them), and remember the backups.

## 8. Troubleshooting

| Symptom | Look at |
|---|---|
| Nothing answers | `docker compose logs jannyq`; is the model reachable (`JANNYQ_LLM_BASE_URL`)? `jannyq_llm_requests_total{result="error"}` |
| Answers are slow | `jannyq_request_duration_seconds`, `jannyq_llm_request_duration_seconds`; `JANNYQ_MAX_CONCURRENT` queues requests |
| "I'm still working on your previous messages" | one chat has more than 4 requests waiting (`jannyq_messages_rejected_total{reason="busy"}`) |
| PDFs are read badly or not at all | the log line "PDFs are read in the sandbox" (or the warning that the sandbox cannot); the sandbox image needs poppler and tesseract |
| Knowledge base is empty | `jannyq_knowledge_files`, the log for "file not indexed" with reasons, the mount of `./knowledge` |
| LINE / Messenger / WhatsApp do not receive | the webhook URL in the platform's console, the secret, `jannyq_http_requests_total{class="4xx"}`; a 401 means a wrong secret |
| Discord stops at start | the close code in the error: usually the MESSAGE CONTENT intent is off |
| `/readyz` is 503 | the body says which check failed (data directory permissions, knowledge database) |

## 9. Without Docker (systemd)

`deploy/systemd/` has two units: `jannyq.service` (the bot, as an unprivileged user with the usual systemd restrictions) and
`jannyq-sandbox.service` (the executor, which must start as root to give each chat its own user, and keeps only the capabilities it needs).
Install the static binary at `/usr/local/bin/jannyq`, create the user (`useradd --system --home /var/lib/jannyq jannyq`), put the
settings in `/etc/jannyq/jannyq.env` (mode 0600, owned by root) and `systemctl enable --now`. Check the result with
`systemd-analyze security jannyq.service`. The units are examples that were not run here.
