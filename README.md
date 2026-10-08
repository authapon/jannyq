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
  - `read_attachment` and `search_attachment` — read long documents that users sent page by page, and find
    passages in them (see [Pictures, PDFs and text files](#pictures-pdfs-and-text-files)).
- **Pictures, PDFs and text files**: users can send them on Telegram and in the web chat. The main model looks
  at pictures itself (so it must be a vision model); PDFs are read in the sandbox, with OCR for scans.
- **Knowledge base (RAG)**: a folder of text and PDF files shared by every chat, searched with `knowledge_search`;
  the index follows edits, deletions and renames (see [Knowledge base](#knowledge-base)).
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
- **Channels**: Telegram and Discord (they call out: no public URL needed), LINE, Messenger and WhatsApp (webhooks, need
  public HTTPS), a **web chat** page, and a terminal channel for local testing.
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
| `--knowledge-dir` | – | folder for the shared knowledge base; enables `knowledge_search` ([details](#knowledge-base)) |
| `--embed-model` | – | embedding model for semantic search (`bge-m3`, `nomic-embed-text`, …); empty = words only |
| `--metrics-listen` / `--metrics-token` | – | Prometheus metrics on their own address ([guide](docs/DEPLOYMENT.md#4-monitoring)) |
| `--backup-dir` / `--backup-interval` / `--backup-keep` | – / `24h` / `7` | automatic backups ([guide](docs/DEPLOYMENT.md#5-backups)) |
| `--retention-days` | `0` | delete chats idle for this many days (`0` = keep) |
| `--attachments` | `true` | let users send pictures, PDFs and text files ([details](#pictures-pdfs-and-text-files)) |
| `--vision` | `auto` | can the model see pictures: `auto` (asks Ollama; `on` for `openai`), `on`, `off` |
| `--pdf-engine` | `auto` | read PDFs in the `sandbox` (when `--sandbox-url` is set), or `native` (in-process, no OCR) |
| `--ocr-langs` | `auto` | Tesseract languages for scans, e.g. `eng+tha`; `off` disables OCR |
| `--attach-max-mb` / `--attach-per-message` / `--attach-rate` | `20` / `5` / `10` | per file, files per message, files per user per minute |
| `--telegram-token` | – | enables the Telegram channel (prefer the env var) |
| `--discord-token` | – | enables the Discord channel (prefer `JANNYQ_DISCORD_TOKEN[_FILE]`; [notes](#discord-notes)) |
| `--line-channel-secret` / `--line-channel-token` | – | enable the LINE channel; needs `--listen` and a public HTTPS URL ([notes](#line-notes)) |
| `--messenger-page-token` / `-app-secret` / `-verify-token` | – | enable the Messenger channel ([notes](#messenger-and-whatsapp-notes)) |
| `--whatsapp-token` / `-phone-number-id` / `-app-secret` / `-verify-token` | – | enable the WhatsApp channel |
| `--cli` | `false` | enable the terminal channel |
| `--allowed-users` | everyone | comma-separated user IDs or `channel:id` — only these people get answers ([details and examples](#who-may-use-the-bot---allowed-users---allowed-groups)) |
| `--allowed-groups` | – | comma-separated **group** IDs (or `channel:id`) where *everybody* gets answers, listed in `--allowed-users` or not ([details](#who-may-use-the-bot---allowed-users---allowed-groups)) |
| `--group-reply` | `mention` | in groups answer only when mentioned/replied to (`mention`) or always (`all`) |
| `--intro` | `true` | the first time the bot answers in a chat, the **model** introduces itself (it reads the system prompt, so it describes the tools and skills you configured, and writes in `--lang`); the introduction is saved in the history like any reply (the answer to the same message follows as a second message, except for Telegram's automatic `/start` with `--commands=false`, which gets the introduction alone), costs one extra model call per chat, and `false` turns it off |
| `--commands` | `true` | chat commands `/help`, `/reset`, `/compact`; `false` removes **all** slash commands: such text is just a message for the model, and the web chat hides its **New chat** button |
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

### Who may use the bot (`--allowed-users`, `--allowed-groups`)

By default **anyone** who can reach the bot can talk to it (and, with `run_command` on, make it run sandboxed commands). Set
`--allowed-users` (env `JANNYQ_ALLOWED_USERS`) to a comma-separated list to let only those people use it.

**How entries are matched**

| Entry | Matches |
|---|---|
| `123456789` | the user with that ID on **any** channel |
| `telegram:123456789` | that ID on that channel only (`telegram`, `discord`, `line`, `messenger`, `whatsapp`, `cli`) |

- Matching is exact and ignores upper/lower case. There are no wildcards and no ranges. Spaces around entries and empty entries are ignored.
- It is a list of **people**, not of chats: a group is not "allowed" by this list; each person speaking in it is checked (to allow a whole group, see `--allowed-groups` below).
- The check comes before the rate limit and before anything is stored or any model is called. Empty (the default) means everyone.
- Prefer `channel:id` once you run several channels: a bare ID would also match an unrelated person whose ID on another platform happens to be the same.

**What a user's ID is, per channel**

| Channel | ID | Where to find it |
|---|---|---|
| Telegram | the numeric user ID, e.g. `123456789` (not the @username) | message `@userinfobot`, or look in the log (below) |
| Discord | the user's snowflake ID, e.g. `80351110224678912` | *User Settings → Advanced → Developer Mode*, then right-click the user → *Copy User ID* |
| LINE | the `U…` user ID (33 characters) | the log (below); LINE does not show it in the app |
| Messenger | the page-scoped ID (PSID) of the sender | the log (below) |
| WhatsApp | the phone number in international format **without `+`**, e.g. `66812345678` | the number itself |
| Web chat | a random ID per visitor (cookie) | cannot be listed — see below |
| CLI | `local` | – |

**The easy way to find an ID:** start the bot, ask the person to send it one message, and read the log. A user who is not on the list
produces

```
level=INFO msg="message from user that is not allowed" channel=telegram user=123456789
```

and gets the reply *"Sorry, you are not allowed to use this bot."* (in the bot's language). Copy `channel` and `user` into the list and restart.

**Examples**

```bash
# 1. Only you, on Telegram (a private bot)
JANNYQ_ALLOWED_USERS=telegram:123456789

# 2. A family: three people on Telegram, one on WhatsApp
JANNYQ_ALLOWED_USERS=telegram:123456789,telegram:987654321,telegram:555000111,whatsapp:66812345678

# 3. The same person on several platforms: list each ID (they differ per platform)
JANNYQ_ALLOWED_USERS=telegram:123456789,discord:80351110224678912,line:U4af4980629abcdef0123456789abcdef

# 4. One channel only, and the IDs are unambiguous, so no prefix is needed
JANNYQ_ALLOWED_USERS=123456789,987654321

# 5. Everyone (the default) - leave it empty or unset
JANNYQ_ALLOWED_USERS=

# 6. As a flag
jannyq --allowed-users telegram:123456789,discord:80351110224678912

# 7. Terminal testing only: the CLI user is "local"
JANNYQ_ALLOWED_USERS=cli:local
```

In `docker-compose.yml` / `.env` it is one line, without quotes or spaces needed:

```env
JANNYQ_ALLOWED_USERS=telegram:123456789,telegram:987654321
```

**Groups.** Only the listed people get answers in a group (unless the group itself is listed in `--allowed-groups`, below). If someone who is not listed mentions the bot there, the bot replies
with the "not allowed" message to that person (it does not answer the question, and that message is not stored). What unlisted people
say in the group *without* addressing the bot is **not kept either**: the model's picture of the conversation (`--group-context all`)
contains only the listed people's messages, so it will not know what the others said. With `--group-reply all` the bot checks every message, so an unlisted member would receive the refusal for each
message they write: use `--group-reply mention` (the default) in groups with unlisted members.

**Whole groups (`--allowed-groups`).** Listing every member of a group in `--allowed-users` is tedious. `--allowed-groups`
(env `JANNYQ_ALLOWED_GROUPS`) names **groups** instead: in a listed group the bot talks to and answers **everybody**, whether or not they are in
`--allowed-users`, and keeps everything said there as context.

- Entries are group chat IDs, written `id` (any channel) or `channel:id` (that channel only), compared exactly but ignoring case, like users.
- The two lists **add up**: a message is answered if its sender is in `--allowed-users` **or** it was sent in a group in `--allowed-groups`.
  Listed users are still answered everywhere (in private and in any other group); other groups are decided by `--allowed-users` as before.
- A group entry never opens a *private* chat: a person who is only in a listed group cannot message the bot privately.
- If you set `--allowed-groups` and leave `--allowed-users` empty, the bot answers **only inside the listed groups** (private chats are refused).
  Only when **both** are empty is everyone answered.
- It applies to groups only (Telegram groups and supergroups, Discord server channels, LINE groups and rooms). Messenger, WhatsApp and the web chat have
  no groups.
- The usual group rules still apply on top: with `--group-reply mention` (default) the bot answers only when mentioned or replied to; with
  `--group-reply all` it answers every message of a listed group, and nobody there gets a refusal.

| Channel | Group ID | Where to find it |
|---|---|---|
| Telegram | a negative number, e.g. `-1001234567890` (supergroups start with `-100`) | add the bot to the group and @mention it from an unlisted account: the log shows `chat=…`; or `@RawDataBot` |
| Discord | the **channel** ID (each text channel is its own chat), e.g. `1098765432109876543` | *Developer Mode*, right-click the channel → *Copy Channel ID*; or the log |
| LINE | the group ID `C…` (rooms: `R…`) | the log (below) |

The easy way to get a group ID is the same as for users: mention the bot in the group before it is listed. The log line now carries the chat:

```
level=INFO msg="message from user that is not allowed" channel=telegram user=123456789 chat=-1001234567890 group=true
```

```bash
# 8. Everybody in one Telegram group may use the bot; nobody else may (private chats are refused)
JANNYQ_ALLOWED_GROUPS=telegram:-1001234567890

# 9. You (anywhere) plus everybody in the family group
JANNYQ_ALLOWED_USERS=telegram:123456789
JANNYQ_ALLOWED_GROUPS=telegram:-1001234567890

# 10. A team: two Telegram groups and one Discord channel, plus two admins who may also use it privately
JANNYQ_ALLOWED_USERS=telegram:123456789,discord:80351110224678912
JANNYQ_ALLOWED_GROUPS=telegram:-1001234567890,telegram:-1009876543210,discord:1098765432109876543

# 11. As flags
jannyq --allowed-users telegram:123456789 --allowed-groups telegram:-1001234567890
```

Being in a listed group is enough, so **anyone who can join that group can use the bot** (and, with `run_command` on, run sandboxed commands):
keep such groups private or invite-only.

**Other things to know**

- **The web chat and the lists do not combine.** Web visitors get a random ID and are never in a group, so no list can name them: as soon as either list is not
  empty, every web visitor is refused. To restrict the web chat use `--web-access-code` (everyone who knows the code may chat) instead,
  or leave the web channel off.
- **Webhook channels (LINE, Messenger, WhatsApp) still receive the message.** The refusal is sent back, but the signed webhook is accepted as
  usual; the list controls who gets service, not who can reach the URL.
- **Changing the list needs a restart** (it is read at start-up).
- **It is not a login.** IDs are what the platform reports; the list is as safe as the platform's own account security. For sensitive
  data also keep `run_command` (`--run-command=off`) and `--knowledge-dir` content in mind: anyone on the list can ask the bot about
  the shared knowledge base.
- **Combine it with the rest:** `--rate-limit` (per allowed user per minute), `--web-access-code` for the web chat, and a private bot
  token (Telegram/Discord bots can be found by anyone, which is why the list is worth having).

### Telegram notes

- In groups the bot answers when it is **@mentioned**, when someone **replies to its message**, or on a
  `/command`. With BotFather's privacy mode on (default) it only receives those messages anyway.
- Commands: `/help`, `/reset` (forget this chat), `/compact` (summarise older messages now). Start the bot with
  `JANNYQ_COMMANDS=false` to have none: chats are then only ever cleared by the operator (retention, deleting the folder);
  automatic compaction still happens.
- Replies are plain text for now (no Markdown rendering).

### Discord notes

1. Create an application at <https://discord.com/developers/applications>, add a **Bot**, copy its token into `JANNYQ_DISCORD_TOKEN`.
2. Under *Bot → Privileged Gateway Intents* switch on **MESSAGE CONTENT INTENT**. Without it Discord hides the text of messages
   that do not mention the bot; the bot refuses to start with a clear message when the intent is missing.
3. Invite it with the `bot` scope and the permissions *View Channels*, *Send Messages*, *Read Message History* (and *Send Messages in Threads*).
- **No public URL**: the bot opens the gateway connection itself (it reconnects and resumes after drops).
- In servers the bot answers when it is **@mentioned** or when someone **replies to its message**. Every other message is kept as context
  (`--group-context all`, with speaker names) but not answered. In direct messages it answers everything.
- Commands: Discord keeps `/…` for slash commands, so write **`!reset`**, `!help`, `!compact` (or mention the bot: `@bot /reset`).
- Pictures, PDFs and text files attached to a message are read like on Telegram. Replies never ping anyone (`allowed_mentions` is empty).

### LINE notes

1. In the [LINE Developers console](https://developers.line.biz/) create a **Messaging API** channel. Copy the *channel secret* and the
   *channel access token* into `JANNYQ_LINE_CHANNEL_SECRET` and `JANNYQ_LINE_CHANNEL_TOKEN`.
2. LINE calls your bot, so it needs a **public HTTPS URL**: use `docker-compose.public.yml` and set the channel's *Webhook URL* to
   `https://<JANNYQ_DOMAIN>/webhook/line` (`--line-webhook-path`), switch on *Use webhook*, and in the *LINE Official Account Manager* turn off
   *auto-reply messages* and *greeting messages*, and allow the bot to join group chats if you want that.
- Every request is checked against the channel secret (HMAC-SHA256, constant time) before its body is read; redelivered events are ignored;
  the webhook answers at once and the model works afterwards, because LINE gives up quickly. Addresses that keep sending bad signatures are turned away.
- In **groups and rooms** the bot answers when it is **@mentioned** (the mention is cut out of the text); other messages are kept as context.
  In one-to-one chats it answers everything. People in a group who have not agreed to share their profile are shown as `user`.
- **Replies use the free reply token** of the message for the first answer; anything after that (or an answer that takes more than about
  50 seconds) is **pushed**, which counts against the monthly message quota of your LINE account. Long answers are split into several messages.
- Pictures and files are read like on Telegram (videos, audio, stickers and locations are not). While the model works, LINE's loading animation is shown in one-to-one chats.

### Messenger and WhatsApp notes

Both use Meta's Graph API and call a webhook of the bot, so they need a **public HTTPS URL** (`docker-compose.public.yml`) and
`--listen`. Create an app at <https://developers.facebook.com/apps>, then:

- **Messenger** (a Facebook Page): add the *Messenger* product, generate a **Page access token** (`JANNYQ_MESSENGER_PAGE_TOKEN`), copy the
  **app secret** (*App settings → Basic*, `JANNYQ_MESSENGER_APP_SECRET`), invent a **verify token** (`JANNYQ_MESSENGER_VERIFY_TOKEN`), and
  set the webhook to `https://<host>/webhook/messenger` with the `messages` field subscribed for the Page.
- **WhatsApp** (Cloud API): add the *WhatsApp* product, create a permanent **access token** of a system user (`JANNYQ_WHATSAPP_TOKEN`), copy the
  **phone number ID** of the business number (`JANNYQ_WHATSAPP_PHONE_NUMBER_ID`; it is not the phone number), the **app secret**
  and an invented **verify token**, and set the webhook to `https://<host>/webhook/whatsapp` with the `messages` field subscribed.
- Meta checks the URL with a handshake (answered with the verify token) and then signs every request with the app secret
  (`X-Hub-Signature-256`, HMAC-SHA256); unsigned or wrongly signed requests are refused before their body is read, redeliveries are ignored,
  and the webhook answers at once while the model works afterwards.
- These are **one-to-one chats**: every message is answered (Messenger's Page inbox and WhatsApp's Cloud API have no groups). Names come from
  the Graph API when the app may read them (Messenger needs permission; WhatsApp sends the profile name), otherwise people are called `user`.
- **Pictures, PDFs and text files** are read like on Telegram. The access token is sent only to Meta, never to the file links of Messenger
  attachments. Videos, audio, stickers, locations and contacts are not read.
- Meta's **24-hour rule**: you can answer within 24 hours of the user's last message; a reply that comes later is refused by Meta (logged). WhatsApp
  also requires an app in *live* mode and, for numbers other than the test number, a verified business.
- Long answers are split (2,000 characters on Messenger, 4,096 on WhatsApp). While the model works, Messenger shows its typing bubble and
  WhatsApp marks the message as read with a typing indicator.

### Choosing a model

The model must support **tool calling** for `web_search`/`web_fetch` to work (for Ollama: qwen3, llama3.1+,
mistral-nemo, …). If a model rejects tools, jannyq logs a warning and keeps chatting without them.
Always set `--context-size` for Ollama: its default context is small and long pages would be silently cut.

## Pictures, PDFs and text files

Send a photo, a PDF or a text file (with or without a caption) and the model reads it. What happens to each:

| File | What the model gets |
|---|---|
| **Picture** (JPEG, PNG, GIF, WebP) | The picture itself, for the main model to look at. It is turned upright (EXIF), shrunk to `--image-max-edge` (1568 px), flattened onto white and re-encoded as JPEG, which also drops its metadata (GPS, camera). Needs a vision model: with `--vision=auto` jannyq asks Ollama (`/api/show`); for `--llm-provider=openai` it assumes yes, so use `--vision=off` if your model cannot see. Without vision, pictures are refused with a clear message. |
| **PDF** with text | The extracted text, page by page. Up to `--attach-inline-chars` (6000) it is shown in the message itself; longer documents are listed with their size and the model reads them with `read_attachment` (page by page) or finds passages with `search_attachment`. |
| **Scanned PDF** (no text layer) | The text recognised with Tesseract (`--ocr-langs`, default `eng`, plus `tha` for `--lang th`), and, for a vision model, pictures of the first `--vision-pages` (3) pages. A scan with neither OCR nor vision is refused. |
| **Text file** (txt, md, csv, json, code, …) | Decoded (UTF-8, UTF-16, or Windows-874 for Thai) and treated like a PDF's text. |

Things to know:

- **Types are decided by content**, not by the file name or the type the app claims. Anything else (zip, exe, …) is refused.
- **PDFs are untrusted input to a large parser**, so jannyq has the **sandbox** read them (`pdfinfo`, `pdftotext`, `pdftoppm`,
  `tesseract` in the sandbox image) instead of parsing them next to its own secrets. With `--pdf-engine=auto`
  that happens whenever `--sandbox-url` is set — **even with `--run-command=off`** — and the sandbox has the tools;
  otherwise a built-in pure-Go reader is used (no OCR, no page pictures; a warning is logged). `--pdf-engine=sandbox` never falls back.
- Text from a file is **data, never instructions**: it is framed between markers, look-alike markers and message
  headers inside it are neutralised, and the system prompt tells the model not to obey it.
- **Only the latest pictures are sent**: the `--image-messages` (3) most recent messages with pictures keep them; older ones become
  a note ("no longer shown"), because a picture costs far more context than its description. Pictures count towards compaction.
- Everything is stored in the chat's own directory (`<data-dir>/sessions/…/files/`), limited to `--attach-chat-mb`
  (200) per chat — the oldest files go first — and removed by `/reset`. With `run_command` on, a copy is placed in the
  chat's workspace as `inbox/<id>-<name>` (`--attach-inbox`) so commands can work on it.
- **Limits**: `--attach-max-mb` (20) per file, `--attach-per-message` (5), `--attach-rate` (10 per user per minute),
  `--pdf-max-pages` (200), `--ocr-max-pages` (15), pictures over 40 megapixels are refused before decoding.
  An unaddressed group message with a file records only that a file was sent; it is not downloaded or opened.
- **Telegram**: photos and documents (bot API limit 20 MB); albums arrive as one message. **Web chat**: 📎 button, drag and drop or paste;
  `--web-max-upload-mb` (10) and `--web-max-files` (4). Other platforms follow with their channels.
- Turn it all off with `--attachments=false`.

## Knowledge base

Point `--knowledge-dir` at a folder and everyone who talks to the bot can ask about its documents. The model calls
`knowledge_search` (passages with file names and pages), `knowledge_read` (read on from a passage) and `knowledge_files` (what is there), then answers and names its sources.

- **Files**: PDF (including scans, by OCR in the sandbox), `txt md rst csv tsv json jsonl xml yaml toml ini log srt vtt tex`.
  Sub-folders are read; hidden files and folders, symbolic links, and other file types are ignored.
- **Search is hybrid**: a full-text index (SQLite FTS5 with trigrams, so Thai, Chinese and other languages written without spaces work)
  and, when `--embed-model` is set, embeddings (Ollama `/api/embed` or an OpenAI-compatible `/embeddings`); the two rankings are merged
  (reciprocal rank fusion) and vector matches below `--knowledge-min-similarity` are dropped. Without an embedding model — or while
  it is unreachable — search works by words only, and the answer says so. Good choices for Ollama: `bge-m3` (multilingual, Thai included)
  or the smaller `nomic-embed-text`. Changing `--embed-model` re-embeds the stored passages without re-reading the files.
- **Lists and sections that run across passages**: documents are cut into passages of about `--knowledge-chunk-chars` (1200) characters, so a
  list can end up half in one passage and half in the next (a curriculum's nine learning outcomes, say, with the model answering with the
  first five). Search results therefore say where each passage sits (`passage 44 of 487`), and **for the two best hits the rest of
  their section is added automatically**: the passages back to where the section opens (`[1, before]`) and on to where the next section
  of the same or a higher rank begins (`[1, continued]`). Sections are found from the headings in the text: Markdown `#` headings and
  numbered ones such as `1.5.3 Learning outcomes` or `2.1) Admission` (at least two levels, so list items like `1.` or `2)` are not headings).
  This reads the stored passages, so existing indexes work without being rebuilt. At most `--knowledge-expand-chars` (3000, `0` = off)
  characters are added per hit, and a text without headings gets just the next passage. When a section is longer than that, the model
  has `knowledge_read` to read on from any passage number, and is told to do so when a list or table is still cut off.
- **How the model is made to use it**: the system prompt lists the documents (file name plus the first lines of each, as a title) and
  tells the model to call `knowledge_search` *first* for anything those documents could cover, or for terms it does not recognise. A
  model told only "there is a knowledge base" often answers from memory instead. Some models cannot call tools at all (the log says
  `model does not support tools`); for them `--knowledge-prefetch auto` (default) searches the knowledge base itself with the user's
  message and puts the passages in front of the model. `always` does that for every message even with tool-capable models (more
  reliable with small models, costs a search per message and may add unrelated passages), `off` never does. If the bot still answers
  from memory: use a model that supports tools, set `--knowledge-prefetch always`, and add an embedding model (`--embed-model`).
- **Built before the bot starts**: at start-up the bot first checks the whole folder and builds or updates the index — reading new and
  changed files, forgetting deleted ones, embedding the passages, waiting for files that are still being copied — and only then starts
  the channels and the web server. The log shows `still building the knowledge base` every 15 s. A big folder with OCR can take a long
  while; `--knowledge-startup-sync=false` (`JANNYQ_KNOWLEDGE_STARTUP_SYNC=false`) builds it in the background instead, with the bot
  answering from what is indexed so far. If the embedding model is unreachable the start is not blocked: those files are searchable by
  words until the background scans embed them (a warning says how many).
- **The index follows the folder**: every `--knowledge-interval` (30 s) the folder is scanned. New files are read, edited files are read again,
  deleted files are forgotten, a renamed file keeps its passages (found by content hash, no new embedding), a file touched without
  a change costs nothing, and a file that changed a moment ago waits until it has settled (so a copy in progress is not indexed half-way).
  A file that cannot be read (damaged, password protected, too large) is listed with the reason and retried when it changes.
  If the folder is missing at a scan, the index is kept rather than emptied.
- **Where**: passages live in `<data-dir>/knowledge.db` (`--knowledge-db`), a single file you can delete to rebuild. The first scan
  runs in the background; until it finishes, answers say that the index is incomplete.
- **Shared by everyone** who can reach the bot — put in it only what all of them may read. Passages are given to the model as data, never instructions.
- **Limits**: `--knowledge-max-file-mb` (50), `--knowledge-pdf-max-pages` (1000), `--knowledge-ocr-pages` (100; `0` = no OCR for the
  knowledge base). Embeddings are searched in memory, which is comfortable up to a few hundred thousand passages (a 1024-dimension
  model needs about 4 KB per passage); passage size is `--knowledge-chunk-chars` (1200) with `--knowledge-overlap` (150).
- **Docker**: `docker-compose.yml` mounts `./knowledge` at `/knowledge` read-only, and the `ollama-pull` helper downloads
  `JANNYQ_EMBED_MODEL` (`bge-m3` in `.env.example`). Drop files into `./knowledge` and wait half a minute.

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

## Operating it

**[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** (Thai summary: [DEPLOYMENT.th.md](docs/DEPLOYMENT.th.md)) is the guide for running jannyq for other people: a pre-exposure checklist,
monitoring, backups and restore drills, upgrades, data retention and a systemd variant. The tools it describes:

- **Metrics** (`--metrics-listen`, `--metrics-token`): Prometheus text format on its own port, with request, model, tool, file, knowledge-base and backup figures.
- **Health**: `/healthz` and `/readyz` on the web server (the data directory is writable, the knowledge database answers).
- **Backups**: `--backup-dir` (daily, newest `--backup-keep` kept, `--backup-files`), and `jannyq backup`, `jannyq verify FILE`, `jannyq restore --from FILE [--force]`.
  Snapshots are consistent while the bot runs; a restore is checked completely before it touches anything.
- **Retention**: `--retention-days N` deletes chats (messages and files) idle for N days.
- **The console log** (`--log-level info` by default, `--log-json` for JSON) shows what the bot is doing, never what people write:

  ```
  level=INFO msg="ready: waiting for messages" channels=2
  level=INFO msg="user connected" channel=telegram user=123456789 name="Ann Lee" chat=123456789 group=false
  level=INFO msg="message received" channel=telegram chat=123456789 user=123456789 group=false addressed=true chars=34 files=0
  level=INFO msg="tool call" tool=knowledge_search chat=telegram:123456789 user=123456789 duration=41ms bytes=5230
  level=INFO msg=reply channel=telegram chat=123456789 user=123456789 result=ok delivered=true chars=412 took=8.2s
  ```

  *user connected* is the first message of a user since the bot started (for the web chat, a visitor opening the page, with a short
  hash instead of the visitor id: `web visitor connected`, `web chat opened`/`closed`). Messages turned away are logged too
  (`message from user that is not allowed`, `message refused: rate limit`, `webhook refused: bad signature`), as are commands, the
  introduction, tool calls, indexing, backups, channel start and stop and shutdown. Texts, answers and tool arguments are not logged.
  Group messages that are only kept as context appear at `--log-level debug`, together with every model request (tokens, timing).

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
handshake, `Dedupe` to ignore redelivered events and `Failures` to turn away addresses that keep failing. Channels receive the shared `server.Server`
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
internal/tool       web_search, web_fetch, run_command, load_skill, read/search_attachment, SSRF-safe HTTP client
internal/knowledge  shared knowledge base: chunking, SQLite store (FTS5 + vectors), hybrid search, folder indexer
internal/attach     pictures (resize, EXIF), PDF text/OCR (sandbox or pure Go), text-file decoding
internal/sandbox    command executor (limits, per-chat users), HTTP server and client
internal/skill      skills loader
internal/audit      command audit log
internal/ratelimit  sliding-window rate limiter
internal/channel    Channel interface; telegram, discord (gateway), line, messenger, whatsapp (webhooks; meta = shared Graph client and receiver), web (page, SSE hub, sessions), cli
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
4. ✅ **Pictures, PDFs and text files** — vision through the main model, PDFs read in the sandbox (OCR for scans),
   Telegram photos/documents/albums and web uploads.
5. ✅ **Knowledge base (RAG)** from a folder of text/PDF files: SQLite, hybrid vector + full-text search, live sync
   of edits, deletions and renames, `knowledge_search` tool.
6. ✅ **Discord and LINE** — gateway and webhook channels with attachments, mentions and group context.
7. ✅ **Messenger and WhatsApp** — Meta webhooks with signature checks, attachments and the same router as every other channel.
8. ✅ **Hardening and operations** — Prometheus metrics, `/readyz`, consistent backups with verified restore, retention, `govulncheck` in CI,
   deployment guide and systemd units. (A container-per-chat Docker backend was left out on purpose: it needs the Docker socket inside the bot;
   see the guide for gVisor or a separate machine instead.)

## Security notes

- `web_fetch` refuses non-public addresses and ignores proxy environment variables on purpose.
- Files sent by users are untrusted: types are detected from content, pixel counts are checked before decoding,
  PDFs are parsed in the sandbox, and their text is framed as data. Don't set `--pdf-engine=native` for a public bot.
- The knowledge base is shared by everyone who can talk to the bot; its passages reach the model as data. The folder is opened
  as a root (links are not followed), so a link placed in it cannot expose other files.
- Content returned by tools is untrusted; the system prompt tells the model not to follow instructions in it.
- Don't expose the bot to everyone without `--rate-limit`; model time is the expensive resource.
- Keep `JANNYQ_SANDBOX_TOKEN` secret and out of the sandbox container's volumes; rotate it if it leaks.
