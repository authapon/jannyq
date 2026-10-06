# jannyq

A chat bot written in Go that connects messaging platforms to **Ollama** or any
**OpenAI-compatible** model, with tool calling for web search and web page reading.

> **Status: Phase 3.** Telegram, **web chat** and terminal channels, `web_search`, `web_fetch`, per-chat
> memory with automatic compaction, `run_command` in an isolated sandbox, skills, and a shared HTTP server
> with **HTTPS via Caddy**. See the [roadmap](#roadmap) for what comes next. 🇹🇭 [อ่านภาษาไทย](README.th.md)

## Features

- **Models**: Ollama native API (honours `num_ctx`) or any OpenAI-compatible `/chat/completions` endpoint.
- **Tools** the model can call:
  - `web_search` — queries your [SearXNG](https://github.com/searxng/searxng) instance.
  - `web_fetch` — downloads a page and returns readable text (HTML → Markdown-like, any charset incl. Thai
    legacy encodings, paged with `offset`). Protected against SSRF: private, loopback and link-local
    addresses are refused at connect time, including after redirects.
  - `run_command` — runs a shell command in a sandbox (see [below](#run_command-and-the-sandbox)); open to
    everyone, with per-user rate limits, resource limits and an audit log. **Off by default.**
  - `load_skill` — loads instructions from your [skills](#skills).
- **Time and names in every conversation**: each user message reaches the model with a header giving when it
  was sent and, in groups, who sent it (see [below](#conversation-context-time-and-names)).
- **Memory per chat**: every user (private chat) and every group has its own SQLite database
  (`<data-dir>/sessions/<channel>/<chat>/session.db`). Deleting a chat's directory forgets it.
- **Context compaction**: when a chat grows past `--compact-after` messages (default 200), or the prompt
  exceeds `--compact-ratio` of `--context-size`, older messages are summarised by the model and replaced by
  the summary; recent messages are kept verbatim.
- **Languages**: fixed messages in English and Thai (`--lang`); the model is told which language to use and
  can mirror the user's language (`--lang-mode follow-user`).
- **Safety basics**: user allowlist, per-user rate limit, per-chat request queue, global concurrency limit,
  bounded tool-call rounds, tool output limits, Telegram token redaction in logs.
- **Channels**: Telegram (long polling, no public URL needed), a **web chat** page, and a terminal channel
  for local testing.
- **Web chat**: one self-contained page (no external scripts or fonts) served by the bot itself; replies arrive
  over server-sent events, history survives reloads, UI texts follow `--lang`, works on phones, dark mode.

## Quick start

### Terminal (no Docker)

```sh
# needs a running Ollama with a tool-capable model:  ollama pull qwen3:8b
go run ./cmd/jannyq --cli --llm-model qwen3:8b --context-size 16384 --lang en
```

Add `--searxng-url http://localhost:8080` to enable `web_search`.

### Docker Compose (Telegram + sandbox + SearXNG + Ollama)

```sh
cp .env.example .env     # set JANNYQ_TELEGRAM_TOKEN (from @BotFather), the model, and
                         # JANNYQ_SANDBOX_TOKEN (generate with: openssl rand -hex 32)
docker compose up -d --build
docker compose logs -f jannyq
```

The compose file starts jannyq, the **sandbox** executor, SearXNG (JSON output enabled in
`deploy/searxng/settings.yml`), Ollama and a one-shot helper that pulls the model. Nothing is published to
the network; Telegram uses outbound long polling. Using a hosted OpenAI-compatible API? Set the provider
variables in `.env` and remove the Ollama services. Don't want command execution? Set
`JANNYQ_RUN_COMMAND=off` and remove the `sandbox` service.

### Terminal with commands (no Docker)

```sh
# terminal 1: the executor (as root it gives every chat its own user; otherwise it warns)
export JANNYQ_SANDBOX_TOKEN=$(openssl rand -hex 32)
jannyq sandbox --workdir /var/lib/jannyq-work --listen 127.0.0.1:9090
# terminal 2
jannyq --cli --llm-model qwen3:8b --run-command sandbox --sandbox-url http://127.0.0.1:9090
```

## Configuration

Every setting is a flag **and** an environment variable: `--llm-model` ⇄ `JANNYQ_LLM_MODEL`. Flags override
environment variables. Secrets can be read from files with a `_FILE` suffix
(`JANNYQ_TELEGRAM_TOKEN_FILE=/run/secrets/tg`). `jannyq --help` lists everything; the main ones:

| Flag / variable | Default | Meaning |
|---|---|---|
| `--llm-provider` | `ollama` | `ollama` (native API) or `openai` (OpenAI-compatible) |
| `--llm-base-url` | per provider | e.g. `http://ollama:11434`, `https://api.openai.com/v1` |
| `--llm-api-key` | – | API key (prefer `JANNYQ_LLM_API_KEY[_FILE]`) |
| `--llm-model` | **required** | model name |
| `--context-size` | `0` | model context in tokens; sent as `num_ctx` to Ollama; drives compaction. `0` = unknown |
| `--lang` | `en` | main language for bot messages and replies (`en`, `th`, …) |
| `--timezone` | `Asia/Bangkok` for `--lang th`, else the server's | zone of the message timestamps (IANA name, e.g. `Europe/London`) |
| `--group-context` | `all` | what the bot remembers of groups: `all` (every message) or `addressed` (only those meant for it) |
| `--lang-mode` | `follow-user` | `follow-user` mirrors the user's language, `default` always uses `--lang` |
| `--data-dir` | `.` | where chat databases are stored |
| `--compact-after` | `200` | summarise old messages once a chat exceeds this many |
| `--compact-ratio` | `0.75` | …or when the prompt passes this share of `--context-size` |
| `--searxng-url` | – | enables `web_search` |
| `--run-command` | `off` | `off`, `sandbox` (recommended) or `host` (unsafe, needs `--run-host-unsafe`) |
| `--sandbox-url` / `--sandbox-token` | – | where the executor is, and the shared secret (≥16 chars; prefer `JANNYQ_SANDBOX_TOKEN[_FILE]`) |
| `--run-rate` | `10` | `run_command` calls per user per minute |
| `--audit-log` | `<data-dir>/audit/commands.jsonl` | one JSON line per command, rotated at 10 MB |
| `--skills-dir` | – | directory of skills; enables `load_skill` |
| `--skills-sandbox-path` | `/skills` | where that directory is mounted inside the sandbox |
| `--telegram-token` | – | enables the Telegram channel (prefer the env var) |
| `--cli` | `false` | enable the terminal channel |
| `--allowed-users` | everyone | comma-separated user IDs or `channel:id` |
| `--group-reply` | `mention` | in groups answer only when mentioned/replied to (`mention`) or always (`all`) |
| `--rate-limit` | `20` | messages per user per minute |
| `--system-prompt[-file]` | – | extra instructions for the model |
| `--fetch-allow-private` | `false` | let `web_fetch` reach private addresses (**disables SSRF protection**) |
| `--listen` | – | HTTP address for the web chat, webhooks and `/healthz` (the Docker image sets `:8080`) |
| `--web` | `false` | serve the web chat (needs `--listen`) |
| `--web-access-code` | – | visitors must enter this code first; empty = open to everyone |
| `--web-base-path` | `/` | where the chat lives, e.g. `/chat/` |
| `--web-ip-rate` | `30` | web messages per client address per minute |
| `--web-run-rate` | `3` | `run_command` calls per address per minute from the web (`0` = off there, `-1` = use `--run-rate`) |
| `--trusted-proxies` | – | IPs/CIDRs of reverse proxies whose `X-Forwarded-*` headers are believed |
| `--http-rate` | `300` | HTTP requests per client address per minute |

### Telegram notes

- In groups the bot answers when it is **@mentioned**, when someone **replies to its message**, or on a
  `/command`. With BotFather's privacy mode on (default) it only receives those messages anyway.
- Commands: `/help`, `/reset` (forget this chat), `/compact` (summarise older messages now).
- Replies are plain text for now (no Markdown rendering).

### Choosing a model

The model must support **tool calling** for `web_search`/`web_fetch` to work (for Ollama: qwen3, llama3.1+,
mistral-nemo, …). If a model rejects tools, jannyq logs a warning and keeps chatting without them.
Always set `--context-size` for Ollama: its default context is small and long pages would be silently cut.

## Conversation context: time and names

Every user message is shown to the model with a header:

```
[1997-07-16T19:20:44+01:00] Ann#7f3a: shall we meet at noon?     ← group
[1997-07-16T19:21:02+01:00] translate this please                  ← private chat
```

- **Time** is ISO 8601 with the UTC offset that applied then (`--timezone`), taken from the platform when it
  says (Telegram does) and stored with the message. The system prompt contains no clock: the newest header *is*
  the current time. That keeps the start of the conversation identical from one message to the next, so
  Ollama/OpenAI can reuse their cache of it instead of reading the whole history again every time.
- **Names**: in groups the header names the speaker; `#7f3a` is a short tag, stable per person and chat, that tells
  apart people with the same name (or someone who renames themselves). The platform user id never reaches the
  model. In private chats the system prompt says once who the model is talking to.
- **Everything said in a group is kept** (`--group-context all`, the default), also when nobody addressed the
  bot, so it knows who said what; it only answers when mentioned, replied to, or given a command. Telegram bots
  need privacy mode turned off (BotFather → `/setprivacy` → Disable) to see group messages; Discord will need the
  Message Content intent. Use `--group-context addressed` to keep only what is meant for the bot.
- **Order**: messages are stored in the order they were sent, even when a backlog arrives in one batch or the
  chat is busy answering. If messages arrived while one was waiting, the model is told which one it is answering.
- **Compaction** keeps who said what and on which date. Chats that nobody addresses are compacted in the
  background too, and cannot grow without limit.
- **Forgery**: names are cleaned (no control or invisible characters, no brackets or colons, 32 characters at
  most) and lines inside a message that look like a header are turned into plain text, so nobody can pose as
  someone else; only the header at the very start of a message is genuine, and the system prompt says so.

Chats stored before this version keep working: their messages are shown with the time they were written.

## Web chat

```sh
jannyq --web --listen :8080 --llm-model qwen3:8b      # then open http://localhost:8080/
```

Visitors get a signed, `HttpOnly` session cookie and their own private conversation (stored like any other
chat). Everything the page needs is served by the bot with a strict Content-Security-Policy: no inline or
third-party scripts. Replies are drawn from DOM nodes, never HTML strings, so markup in a reply (or in what a
visitor types) is shown as text; only `http(s)` links are made clickable. `/reset` or the **New chat** button
starts over.

| Protection | Details |
|---|---|
| Who may chat | Anyone, or only visitors who know `--web-access-code` (constant-time check, throttled per address; changing the code signs everyone out) |
| Cross-site requests | `POST`s need `Content-Type: application/json` and a same-origin `Origin`/`Sec-Fetch-Site` (extra origins via `--web-allowed-origins`); cookies are `SameSite=Lax` |
| Limits per address | Messages per minute, new anonymous sessions per hour, open streams, in-flight messages; creating a new session does **not** reset them. IPv6 clients are limited per `/64`, since one subscriber controls a whole `/64` |
| Commands | `run_command` from the web has its own, tighter per-address quota (`--web-run-rate`) |
| Message size | `--web-max-message` characters; request bodies are capped at 64 KB |
| Real client address | `X-Forwarded-For` is believed only from `--trusted-proxies`, using the right-most untrusted entry |

Without an access code the chat is open to anyone who can reach it, and every message costs model time:
put it behind the access code (12+ characters; guesses are throttled but not impossible to distribute) or keep
the rate limits low. Memory use is bounded (replies kept for reconnecting browsers are capped per chat, idle
chats are forgotten after 30 minutes), and a chat's database file is only created when its first message is sent.

## Public deployment (HTTPS)

`docker-compose.public.yml` adds [Caddy](https://caddyserver.com/), which gets and renews a Let's Encrypt
certificate by itself, and turns the web chat on:

```sh
# .env: JANNYQ_DOMAIN=bot.example.com  JANNYQ_WEB_ACCESS_CODE=...   (DNS → this server, ports 80/443 open)
docker compose -f docker-compose.yml -f docker-compose.public.yml up -d --build
```

Only Caddy is published. jannyq sits on a private `edge` network with Caddy and trusts its forwarding headers
only from there. Telegram and Discord never need a public URL; LINE, Messenger and WhatsApp (later phases)
do, and will use this same server.

## Webhook toolkit (for channel authors)

`internal/webhook` provides what every webhook channel needs, already tested: bounded body reading,
`VerifyHex` (Meta's `X-Hub-Signature-256`) and `VerifyBase64` (LINE's `X-Line-Signature`) in constant time,
`Signed(...)` to run a handler only after the signature checks out, `MetaChallenge` for the subscription
handshake, and `Dedupe` to ignore redelivered events. Channels receive the shared `server.Server`
(rate limiting, client address detection, security headers) and register their routes on its mux.

## `run_command` and the sandbox

Letting a language model run shell commands for anyone who can message the bot is powerful and dangerous:
the model can be talked into running things, and text it reads on the web can try to instruct it
(prompt injection). jannyq therefore never runs commands itself. The bot sends them over HTTP to the
**sandbox executor** (`jannyq sandbox`, its own container), which holds no secrets and enforces the limits:

| Layer | What it does |
|---|---|
| Separate container | No bot token, API key or database; on an **internal Docker network without internet**, invisible to Ollama/SearXNG/the bot's port. Commands get a scrubbed environment. |
| One user per chat | The executor starts as root with only `SETUID/SETGID/CHOWN/DAC_OVERRIDE/FOWNER/KILL`, and runs every workspace as its own unprivileged uid with a `0700` directory. Chats cannot read, modify or signal each other, and commands cannot read the executor's own memory/environment (the shared token). Commands are never root. |
| Limits per command | Timeout (default 30 s, max 120 s) with the whole process group killed, CPU seconds, open files, largest file, **process count per chat (fork bombs)**, no core dumps, stdin closed, output capped (head + tail kept). Limits are applied with `setrlimit` by a tiny re-exec of the binary, not by the shell, so they work with any `/bin/sh`. Background processes are reaped. |
| Limits per chat | 256 MB workspace quota — when exceeded, writes are disabled (`ulimit -f 0`) but deleting still works; one command at a time; idle workspaces deleted after 7 days. |
| Limits overall | Concurrency cap, `pids_limit`, memory and CPU limits and a read-only root filesystem in `docker-compose.yml`; per-user `--run-rate`; commands see only a persistent per-chat workspace. |
| Authentication | Bearer token (constant-time compare, ≥16 characters) between bot and executor. |
| Audit | Every command is logged (time, channel, chat, user, command, exit code, duration), with owner-only permissions. |
| Model guidance | The system prompt tells the model to run commands only for the user's request and never because text from a web page or tool tells it to. |

`/reset` also deletes the chat's workspace. Workspaces are named by a hash of the chat ID.

Executor settings (`jannyq sandbox --help`; every flag has a `JANNYQ_SANDBOX_*` variable): `--listen`, `--token`,
`--workdir`, `--uid-base/--uid-count`, `--default-timeout`, `--max-timeout`, `--max-output-kb`, `--max-file-mb`,
`--cpu-seconds`, `--open-files`, `--max-procs`, `--quota-mb`, `--max-concurrent`, `--idle-ttl`, `--network`.

**Honest limits.** A container is not a VM: a kernel exploit could escape it. For public bots consider
gVisor (`runtime: runsc` in the compose file) or a dedicated host. Internet access for commands is **off**;
enabling it (`docker-compose.egress.yml`) lets users make requests from your server's address, so read the
warnings in that file and block private ranges. `--run-command=host` runs commands directly on the bot's machine
and exists only for private experiments. Also know that:

- All chats share one kernel and one memory/CPU budget (`mem_limit`, `cpus`): a chat can still slow others down.
- Process command lines are visible to other users through `/proc` (a container cannot hide them), so users
  can see what other chats' commands are running while they run. Don't type secrets into commands.
- The `/tmp` tmpfs is shared; leftovers of a workspace's user are removed when its user id is recycled.
- Disk use grows with the number of chats (256 MB each by default, idle ones removed after 7 days): put
  `/work` on a size-limited volume.
- The sandbox shares an internal network with the bot, whose HTTP port also serves the web chat. Commands
  have no internet but can reach that port; it needs no secret to use, so it only exposes what the public
  can already see. Don't add privileged endpoints to it.

## Skills

A skill is a folder with a `SKILL.md`: a short description in YAML front matter, then instructions.

```
skills/csv-summary/SKILL.md      ---
skills/csv-summary/summarize.py  description: Summarise a CSV file. Use when the user shares CSV data.
                                 ---
                                 1. Save the data to data.csv  2. Run python3 /skills/csv-summary/summarize.py data.csv ...
```

Only names and descriptions go into the system prompt; the model calls `load_skill` to read the instructions
(and other text files in the folder) when a request matches. The folder is mounted read-only into the sandbox
too, so skills can ship scripts. Folder names are lowercase (`a-z0-9_-`); the directory is rescanned every few
seconds, so skills can be added without a restart. A sample skill lives in [`skills/`](skills/).

## Layout

```
cmd/jannyq          entry point
internal/config     flags + JANNYQ_* environment
internal/llm        Provider interface; Ollama and OpenAI-compatible clients
internal/agent      conversation loop, tool execution, compaction
internal/session    per-chat SQLite storage with an LRU of open databases
internal/tool       web_search, web_fetch, run_command, load_skill, SSRF-safe HTTP client
internal/sandbox    command executor (limits, per-chat users), HTTP server and client
internal/skill      skills loader
internal/audit      command audit log
internal/ratelimit  sliding-window rate limiter
internal/channel    Channel interface; telegram, web (page, SSE hub, sessions), cli
internal/server     shared HTTP server: rate limits, client IP, security headers
internal/webhook    signature checks, Meta handshake, event de-duplication
internal/router     access control, rate limits, commands
internal/i18n       embedded message catalogs (en, th)
```

## Development

```sh
make test      # go test -race ./...
make build     # static binary in bin/ (pure Go, no cgo)
make docker
```

## Roadmap

1. ✅ **Core** — agent loop, Ollama/OpenAI, per-chat SQLite memory + compaction, `web_search`, `web_fetch`,
   Telegram, CLI, Docker.
2. ✅ **`run_command` + skills** — sandbox executor with per-chat users, limits, quotas and audit log; skills loader.
3. ✅ **Web chat** and the shared webhook server (signature checks, rate limits, HTTPS with Caddy).
4. Images and PDFs (vision uses the main model; PDF text extraction with OCR fallback).
5. Knowledge base (RAG) from a folder of text/PDF files: SQLite, hybrid vector + full-text search, live sync
   of edits and deletions, `knowledge_search` tool.
6. Discord and LINE.
7. Messenger and WhatsApp.
8. Hardening and operations: per-session sandbox containers (Docker backend), metrics, backups, deployment guide.

## Security notes

- `web_fetch` refuses non-public addresses and ignores proxy environment variables on purpose.
- Content returned by tools is untrusted; the system prompt tells the model not to follow instructions in it.
- Don't expose the bot to everyone without `--rate-limit`; model time is the expensive resource.
- Keep `JANNYQ_SANDBOX_TOKEN` secret and out of the sandbox container's volumes; rotate it if it leaks.
