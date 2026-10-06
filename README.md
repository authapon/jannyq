# jannyq

A chat bot written in Go that connects messaging platforms to **Ollama** or any
**OpenAI-compatible** model, with tool calling for web search and web page reading.

> **Status: Phase 1 (core).** Telegram + terminal channels, `web_search`, `web_fetch`,
> per-chat memory with automatic compaction, Docker packaging. See the
> [roadmap](#roadmap) for what comes next. 🇹🇭 [อ่านภาษาไทย](README.th.md)

## Features (Phase 1)

- **Models**: Ollama native API (honours `num_ctx`) or any OpenAI-compatible `/chat/completions` endpoint.
- **Tools** the model can call:
  - `web_search` — queries your [SearXNG](https://github.com/searxng/searxng) instance.
  - `web_fetch` — downloads a page and returns readable text (HTML → Markdown-like, any charset incl. Thai
    legacy encodings, paged with `offset`). Protected against SSRF: private, loopback and link-local
    addresses are refused at connect time, including after redirects.
- **Memory per chat**: every user (private chat) and every group has its own SQLite database
  (`<data-dir>/sessions/<channel>/<chat>/session.db`). Deleting a chat's directory forgets it.
- **Context compaction**: when a chat grows past `--compact-after` messages (default 200), or the prompt
  exceeds `--compact-ratio` of `--context-size`, older messages are summarised by the model and replaced by
  the summary; recent messages are kept verbatim.
- **Languages**: fixed messages in English and Thai (`--lang`); the model is told which language to use and
  can mirror the user's language (`--lang-mode follow-user`).
- **Safety basics**: user allowlist, per-user rate limit, per-chat request queue, global concurrency limit,
  bounded tool-call rounds, tool output limits, Telegram token redaction in logs.
- **Channels**: Telegram (long polling, no public URL needed) and a terminal channel for local testing.

## Quick start

### Terminal (no Docker)

```sh
# needs a running Ollama with a tool-capable model:  ollama pull qwen3:8b
go run ./cmd/jannyq --cli --llm-model qwen3:8b --context-size 16384 --lang en
```

Add `--searxng-url http://localhost:8080` to enable `web_search`.

### Docker Compose (Telegram + SearXNG + Ollama)

```sh
cp .env.example .env     # set JANNYQ_TELEGRAM_TOKEN (from @BotFather) and the model
docker compose up -d --build
docker compose logs -f jannyq
```

The compose file starts jannyq, SearXNG (JSON output enabled in `deploy/searxng/settings.yml`), Ollama and a
one-shot helper that pulls the model. Nothing is published to the network; Telegram uses outbound long polling.
Using a hosted OpenAI-compatible API? Set the provider variables in `.env` and remove the Ollama services.

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
| `--lang-mode` | `follow-user` | `follow-user` mirrors the user's language, `default` always uses `--lang` |
| `--data-dir` | `.` | where chat databases are stored |
| `--compact-after` | `200` | summarise old messages once a chat exceeds this many |
| `--compact-ratio` | `0.75` | …or when the prompt passes this share of `--context-size` |
| `--searxng-url` | – | enables `web_search` |
| `--telegram-token` | – | enables the Telegram channel (prefer the env var) |
| `--cli` | `false` | enable the terminal channel |
| `--allowed-users` | everyone | comma-separated user IDs or `channel:id` |
| `--group-reply` | `mention` | in groups answer only when mentioned/replied to (`mention`) or always (`all`) |
| `--rate-limit` | `20` | messages per user per minute |
| `--system-prompt[-file]` | – | extra instructions for the model |
| `--fetch-allow-private` | `false` | let `web_fetch` reach private addresses (**disables SSRF protection**) |
| `--listen` | – | HTTP address for `/healthz` (the Docker image sets `:8080`) |

### Telegram notes

- In groups the bot answers when it is **@mentioned**, when someone **replies to its message**, or on a
  `/command`. With BotFather's privacy mode on (default) it only receives those messages anyway.
- Commands: `/help`, `/reset` (forget this chat), `/compact` (summarise older messages now).
- Replies are plain text for now (no Markdown rendering).

### Choosing a model

The model must support **tool calling** for `web_search`/`web_fetch` to work (for Ollama: qwen3, llama3.1+,
mistral-nemo, …). If a model rejects tools, jannyq logs a warning and keeps chatting without them.
Always set `--context-size` for Ollama: its default context is small and long pages would be silently cut.

## Layout

```
cmd/jannyq          entry point
internal/config     flags + JANNYQ_* environment
internal/llm        Provider interface; Ollama and OpenAI-compatible clients
internal/agent      conversation loop, tool execution, compaction
internal/session    per-chat SQLite storage with an LRU of open databases
internal/tool       web_search, web_fetch, SSRF-safe HTTP client
internal/channel    Channel interface; telegram, cli
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
2. `run_command` in an isolated sandbox container (open to everyone, with limits, quotas and audit log) and a
   skills loader.
3. Web chat page and the shared webhook server (signature checks, rate limits, HTTPS profile).
4. Images and PDFs (vision uses the main model; PDF text extraction with OCR fallback).
5. Knowledge base (RAG) from a folder of text/PDF files: SQLite, hybrid vector + full-text search, live sync
   of edits and deletions, `knowledge_search` tool.
6. Discord and LINE.
7. Messenger and WhatsApp.
8. Hardening and operations: per-session sandbox containers, metrics, backups, deployment guide.

## Security notes

- `web_fetch` refuses non-public addresses and ignores proxy environment variables on purpose.
- Content returned by tools is untrusted; the system prompt tells the model not to follow instructions in it.
- Don't expose the bot to everyone without `--rate-limit`; model time is the expensive resource.
