// Package config loads settings from JANNYQ_* environment variables and
// command-line flags. Every flag --some-name has the environment variable
// JANNYQ_SOME_NAME; flags take precedence over the environment, which takes
// precedence over defaults. Secrets can also be read from files via
// JANNYQ_<NAME>_FILE (for Docker/Kubernetes secrets).
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // so that time zones work in minimal containers too
)

// Config holds all settings.
type Config struct {
	// General
	Lang     string
	LangMode string
	// Timezone is an IANA name such as Asia/Bangkok; it sets the offset in the
	// timestamps the model sees. Empty: Asia/Bangkok for --lang th, else the server's.
	Timezone string
	BotName  string
	DataDir  string
	Listen   string
	LogLevel string
	LogJSON  bool

	// Model
	LLMProvider string
	LLMBaseURL  string
	LLMAPIKey   string
	LLMModel    string
	ContextSize int
	Temperature float64 // negative = provider default
	LLMTimeout  time.Duration

	// Conversation
	CompactAfter  int
	CompactRatio  float64
	CompactKeep   int
	MaxSteps      int
	ToolMaxOutput int
	SystemPrompt  string

	// Tools
	SearxngURL        string
	SearchMaxResults  int
	FetchMaxBytes     int64
	FetchTimeout      time.Duration
	FetchAllowPrivate bool

	// Command execution and skills
	RunCommand        string // off, sandbox or host
	SandboxURL        string
	SandboxToken      string
	RunRate           int // commands per user per minute
	RunHostUnsafe     bool
	AuditLog          string
	SkillsDir         string
	SkillsSandboxPath string

	// HTTP server and web chat
	TrustedProxies     []string
	HTTPRate           int
	Web                bool
	WebBasePath        string
	WebTitle           string
	WebAccessCode      string
	WebSecret          string
	WebMaxMessage      int
	WebIPRate          int
	WebSessionsPerHour int
	WebSecureCookies   string
	WebAllowedOrigins  []string
	WebRunRate         int

	// Attachments: pictures, PDFs and text files sent by users
	Attachments       bool
	Vision            string // auto, on or off
	AttachMaxMB       int    // largest file accepted
	AttachPerMessage  int
	AttachRate        int // files per user per minute
	AttachChatMB      int // disk space of one chat's files
	AttachInlineChars int
	AttachInbox       bool // copy files into the run_command workspace
	ImageMaxEdge      int
	ImageMessages     int // latest messages whose pictures are sent to the model
	PDFEngine         string
	PDFMaxPages       int
	OCRLangs          string
	OCRMaxPages       int
	VisionPages       int // pages of a scanned PDF shown as pictures
	WebMaxUploadMB    int
	WebMaxFiles       int

	// Knowledge base
	KnowledgeDir        string
	KnowledgeDB         string
	EmbedModel          string
	EmbedProvider       string
	EmbedBaseURL        string
	EmbedAPIKey         string
	KnowledgeInterval   time.Duration
	KnowledgeChunkChars int
	KnowledgeOverlap    int
	KnowledgeResults    int
	KnowledgeMinSim     float64
	KnowledgeMaxFileMB  int
	KnowledgePDFPages   int
	KnowledgeOCRPages   int

	// Channels
	TelegramToken string
	TelegramAPI   string
	DiscordToken  string
	DiscordAPI    string
	DiscordGW     string
	LineSecret    string
	LineToken     string
	LinePath      string
	LineAPI       string
	LineDataAPI   string

	MessengerToken       string
	MessengerAppSecret   string
	MessengerVerifyToken string
	MessengerPath        string
	WhatsAppToken        string
	WhatsAppPhoneID      string
	WhatsAppAppSecret    string
	WhatsAppVerifyToken  string
	WhatsAppPath         string
	GraphAPI             string
	CLI                  bool

	// Access control and limits
	AllowedUsers    []string
	GroupReply      string
	GroupContext    string
	RateLimit       int
	MaxConcurrent   int
	RequestTimeout  time.Duration
	MaxOpenSessions int
}

// ErrHelp is returned by Load when --help was requested.
var ErrHelp = flag.ErrHelp

type loader struct {
	fs     *flag.FlagSet
	env    func(string) string
	prefix string // environment variable prefix, e.g. JANNYQ_
	errs   []error
}

func (l *loader) envName(flagName string) string {
	return l.prefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// raw returns the environment value for a flag, or "" if unset.
func (l *loader) raw(name string) string { return strings.TrimSpace(l.env(l.envName(name))) }

func (l *loader) usage(name, usage string) string {
	return usage + " [" + l.envName(name) + "]"
}

func (l *loader) str(p *string, name, def, usage string) {
	if v := l.raw(name); v != "" {
		def = v
	}
	l.fs.StringVar(p, name, def, l.usage(name, usage))
}

// secret is like str but also supports JANNYQ_<NAME>_FILE.
func (l *loader) secret(p *string, name, usage string) {
	def := l.raw(name)
	if def == "" {
		if path := l.raw(name + "_file"); path != "" {
			b, err := os.ReadFile(path)
			if err != nil {
				l.errs = append(l.errs, fmt.Errorf("%s_FILE: %w", l.envName(name), err))
			} else {
				def = strings.TrimSpace(string(b))
			}
		}
	}
	l.fs.StringVar(p, name, def, l.usage(name, usage)+" (or "+l.envName(name)+"_FILE)")
}

func (l *loader) integer(p *int, name string, def int, usage string) {
	if v := l.raw(name); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not an integer", l.envName(name), v))
		} else {
			def = n
		}
	}
	l.fs.IntVar(p, name, def, l.usage(name, usage))
}

func (l *loader) int64v(p *int64, name string, def int64, usage string) {
	if v := l.raw(name); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not an integer", l.envName(name), v))
		} else {
			def = n
		}
	}
	l.fs.Int64Var(p, name, def, l.usage(name, usage))
}

func (l *loader) float(p *float64, name string, def float64, usage string) {
	if v := l.raw(name); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not a number", l.envName(name), v))
		} else {
			def = f
		}
	}
	l.fs.Float64Var(p, name, def, l.usage(name, usage))
}

func (l *loader) boolean(p *bool, name string, def bool, usage string) {
	if v := l.raw(name); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not a boolean", l.envName(name), v))
		} else {
			def = b
		}
	}
	l.fs.BoolVar(p, name, def, l.usage(name, usage))
}

func (l *loader) duration(p *time.Duration, name string, def time.Duration, usage string) {
	if v := l.raw(name); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			l.errs = append(l.errs, fmt.Errorf("%s: %q is not a duration (e.g. 30s, 5m)", l.envName(name), v))
		} else {
			def = d
		}
	}
	l.fs.DurationVar(p, name, def, l.usage(name, usage))
}

// Load parses args (without the program name) and the environment.
func Load(args []string, env func(string) string, stderr io.Writer) (*Config, error) {
	c := &Config{}
	fs := flag.NewFlagSet("jannyq", flag.ContinueOnError)
	fs.SetOutput(stderr)
	l := &loader{fs: fs, env: env, prefix: "JANNYQ_"}

	var allowed, systemPromptFile, proxies, origins string

	// General
	l.str(&c.Lang, "lang", "en", "main language for messages and replies (en, th, ...)")
	l.str(&c.LangMode, "lang-mode", "follow-user", "reply language: follow-user (mirror the user) or default (always --lang)")
	l.str(&c.Timezone, "timezone", "", "time zone for message timestamps, e.g. Asia/Bangkok (default: Asia/Bangkok for --lang th, else the server's)")
	l.str(&c.BotName, "bot-name", "Jannyq", "name the bot uses for itself")
	l.str(&c.DataDir, "data-dir", ".", "directory for chat databases and files")
	l.str(&c.Listen, "listen", "", "address for the HTTP server (health check), e.g. :8080; empty disables it")
	l.str(&c.LogLevel, "log-level", "info", "log level: debug, info, warn, error")
	l.boolean(&c.LogJSON, "log-json", false, "log in JSON format")

	// Model
	l.str(&c.LLMProvider, "llm-provider", "ollama", "model API: ollama (native) or openai (OpenAI-compatible)")
	l.str(&c.LLMBaseURL, "llm-base-url", "", "model API base URL (default depends on provider)")
	l.secret(&c.LLMAPIKey, "llm-api-key", "API key for the model endpoint")
	l.str(&c.LLMModel, "llm-model", "", "model name (required)")
	l.integer(&c.ContextSize, "context-size", 0, "context window of the model in tokens; 0 = unknown (sent to Ollama as num_ctx)")
	l.float(&c.Temperature, "llm-temperature", -1, "sampling temperature; negative = model default")
	l.duration(&c.LLMTimeout, "llm-timeout", 5*time.Minute, "timeout of a single model call")

	// Conversation
	l.integer(&c.CompactAfter, "compact-after", 200, "summarise older messages once a chat exceeds this many messages")
	l.float(&c.CompactRatio, "compact-ratio", 0.75, "also compact when the prompt exceeds this share of --context-size")
	l.integer(&c.CompactKeep, "compact-keep", 20, "recent messages kept verbatim when compacting")
	l.integer(&c.MaxSteps, "max-steps", 8, "maximum tool-call rounds per message")
	l.integer(&c.ToolMaxOutput, "tool-max-output", 12000, "maximum characters of a tool result given to the model")
	l.str(&c.SystemPrompt, "system-prompt", "", "extra instructions appended to the system prompt")
	l.str(&systemPromptFile, "system-prompt-file", "", "read extra system prompt instructions from this file")

	// Tools
	l.str(&c.SearxngURL, "searxng-url", "", "SearXNG base URL; enables the web_search tool")
	l.integer(&c.SearchMaxResults, "search-max-results", 8, "maximum web_search results")
	l.int64v(&c.FetchMaxBytes, "fetch-max-bytes", 2<<20, "maximum bytes downloaded by web_fetch")
	l.duration(&c.FetchTimeout, "fetch-timeout", 25*time.Second, "web_fetch timeout")
	l.boolean(&c.FetchAllowPrivate, "fetch-allow-private", false, "let web_fetch reach private/loopback addresses (disables SSRF protection!)")

	// Command execution and skills
	l.str(&c.RunCommand, "run-command", "off", "run_command tool: off, sandbox (recommended) or host (unsafe)")
	l.str(&c.SandboxURL, "sandbox-url", "", "URL of the sandbox executor, e.g. http://sandbox:9090 (run-command=sandbox)")
	l.secret(&c.SandboxToken, "sandbox-token", "shared secret for the sandbox executor")
	l.integer(&c.RunRate, "run-rate", 10, "run_command calls per user per minute; 0 = unlimited")
	l.boolean(&c.RunHostUnsafe, "run-host-unsafe", false, "confirm that run-command=host runs untrusted commands directly on this machine")
	l.str(&c.AuditLog, "audit-log", "", "command audit log file (default <data-dir>/audit/commands.jsonl)")
	l.str(&c.SkillsDir, "skills-dir", "", "directory of skills (folders with SKILL.md); empty disables skills")
	l.str(&c.SkillsSandboxPath, "skills-sandbox-path", "/skills", "where the skills directory is mounted inside the sandbox; empty if it is not")

	// HTTP server and web chat
	l.str(&proxies, "trusted-proxies", "", "comma-separated IPs/CIDRs of reverse proxies whose X-Forwarded-* headers are believed")
	l.integer(&c.HTTPRate, "http-rate", 300, "HTTP requests per client address per minute; 0 = unlimited")
	l.boolean(&c.Web, "web", false, "enable the web chat page (needs --listen)")
	l.str(&c.WebBasePath, "web-base-path", "/", "URL path where the web chat is served, e.g. / or /chat/")
	l.str(&c.WebTitle, "web-title", "", "title of the web chat (default: --bot-name)")
	l.secret(&c.WebAccessCode, "web-access-code", "code visitors must enter before chatting; empty lets anyone chat")
	l.secret(&c.WebSecret, "web-secret", "key that signs web sessions (default: generated once into <data-dir>/web_secret)")
	l.integer(&c.WebMaxMessage, "web-max-message", 4000, "longest web chat message in characters")
	l.integer(&c.WebIPRate, "web-ip-rate", 30, "web chat messages per client address per minute")
	l.integer(&c.WebSessionsPerHour, "web-sessions-per-hour", 20, "new anonymous web sessions per client address per hour")
	l.str(&c.WebSecureCookies, "web-secure-cookies", "auto", "Secure flag of the session cookie: auto (when served over HTTPS), on or off")
	l.str(&origins, "web-allowed-origins", "", "comma-separated extra origins allowed to post to the web chat, e.g. https://example.com")
	l.integer(&c.WebRunRate, "web-run-rate", 3, "run_command calls per client address per minute from the web chat; 0 disables it there, -1 uses --run-rate")

	// Attachments
	l.boolean(&c.Attachments, "attachments", true, "let users send pictures, PDF files and text files")
	l.str(&c.Vision, "vision", "auto", "can the model look at pictures: auto (asks Ollama; on for openai), on or off")
	l.integer(&c.AttachMaxMB, "attach-max-mb", 20, "largest file accepted, in MB")
	l.integer(&c.AttachPerMessage, "attach-per-message", 5, "files read per message")
	l.integer(&c.AttachRate, "attach-rate", 10, "files per user per minute; 0 = unlimited")
	l.integer(&c.AttachChatMB, "attach-chat-mb", 200, "disk space for the files of one chat in MB; the oldest are deleted beyond it")
	l.integer(&c.AttachInlineChars, "attach-inline-chars", 6000, "longest document text shown in the conversation itself; longer documents are read with read_attachment")
	l.boolean(&c.AttachInbox, "attach-inbox", true, "also copy files into the run_command workspace (inbox/) when commands are enabled")
	l.integer(&c.ImageMaxEdge, "image-max-edge", 1568, "pictures are shrunk so that their longer side is at most this many pixels")
	l.integer(&c.ImageMessages, "image-messages", 3, "how many of the latest messages with pictures are sent to the model with their pictures")
	l.str(&c.PDFEngine, "pdf-engine", "auto", "where PDFs are read: auto (the sandbox when --sandbox-url is set), sandbox or native")
	l.integer(&c.PDFMaxPages, "pdf-max-pages", 200, "longest PDF read, in pages")
	l.str(&c.OCRLangs, "ocr-langs", "auto", "Tesseract languages for scanned PDFs, e.g. eng+tha; auto (eng, plus tha for --lang th) or off")
	l.integer(&c.OCRMaxPages, "ocr-max-pages", 15, "pages of one scanned PDF that are recognised")
	l.integer(&c.VisionPages, "vision-pages", 3, "pages of a scanned PDF shown to the model as pictures")
	l.integer(&c.WebMaxUploadMB, "web-max-upload-mb", 10, "largest file the web chat accepts, in MB")
	l.integer(&c.WebMaxFiles, "web-max-files", 4, "files per message in the web chat")

	// Knowledge base
	l.str(&c.KnowledgeDir, "knowledge-dir", "", "folder of text and PDF files for the shared knowledge base (searched with knowledge_search); empty disables it")
	l.str(&c.KnowledgeDB, "knowledge-db", "", "knowledge base database file (default <data-dir>/knowledge.db)")
	l.str(&c.EmbedModel, "embed-model", "", "embedding model for semantic search, e.g. nomic-embed-text or bge-m3; empty searches by words only")
	l.str(&c.EmbedProvider, "embed-provider", "", "API of the embedding model: ollama or openai (default: --llm-provider)")
	l.str(&c.EmbedBaseURL, "embed-base-url", "", "embedding API base URL (default: --llm-base-url)")
	l.secret(&c.EmbedAPIKey, "embed-api-key", "API key for the embedding endpoint (default: --llm-api-key)")
	l.duration(&c.KnowledgeInterval, "knowledge-interval", 30*time.Second, "how often the knowledge folder is scanned for changes")
	l.integer(&c.KnowledgeChunkChars, "knowledge-chunk-chars", 1200, "size of a knowledge passage in characters")
	l.integer(&c.KnowledgeOverlap, "knowledge-overlap", 150, "characters shared by neighbouring passages")
	l.integer(&c.KnowledgeResults, "knowledge-results", 5, "passages returned by a knowledge search")
	l.float(&c.KnowledgeMinSim, "knowledge-min-similarity", 0.25, "least cosine similarity for a semantic match (0-1)")
	l.integer(&c.KnowledgeMaxFileMB, "knowledge-max-file-mb", 50, "largest knowledge file in MB; larger files are skipped")
	l.integer(&c.KnowledgePDFPages, "knowledge-pdf-max-pages", 1000, "longest PDF indexed, in pages")
	l.integer(&c.KnowledgeOCRPages, "knowledge-ocr-pages", 100, "pages of one scanned PDF that are recognised with OCR; 0 turns OCR off for the knowledge base")

	// Channels
	l.secret(&c.TelegramToken, "telegram-token", "Telegram bot token; enables the Telegram channel")
	l.str(&c.TelegramAPI, "telegram-api", "https://api.telegram.org", "Telegram Bot API base URL")
	l.secret(&c.DiscordToken, "discord-token", "Discord bot token; enables the Discord channel (turn on the MESSAGE CONTENT intent in the developer portal)")
	l.str(&c.DiscordAPI, "discord-api", "https://discord.com/api/v10", "Discord REST API base URL")
	l.str(&c.DiscordGW, "discord-gateway", "wss://gateway.discord.gg", "Discord gateway URL")
	l.secret(&c.LineSecret, "line-channel-secret", "LINE channel secret, to check webhook signatures; with --line-channel-token enables the LINE channel (needs --listen and a public HTTPS URL)")
	l.secret(&c.LineToken, "line-channel-token", "LINE channel access token")
	l.str(&c.LinePath, "line-webhook-path", "/webhook/line", "path of the LINE webhook on the HTTP server")
	l.str(&c.LineAPI, "line-api", "https://api.line.me", "LINE Messaging API base URL")
	l.str(&c.LineDataAPI, "line-data-api", "https://api-data.line.me", "LINE content API base URL")
	l.secret(&c.MessengerToken, "messenger-page-token", "Facebook Page access token; with the app secret and verify token enables the Messenger channel (needs --listen and a public HTTPS URL)")
	l.secret(&c.MessengerAppSecret, "messenger-app-secret", "secret of the Meta app, to check Messenger webhook signatures")
	l.secret(&c.MessengerVerifyToken, "messenger-verify-token", "string you choose and enter in the Meta console when subscribing the Messenger webhook")
	l.str(&c.MessengerPath, "messenger-webhook-path", "/webhook/messenger", "path of the Messenger webhook on the HTTP server")
	l.secret(&c.WhatsAppToken, "whatsapp-token", "WhatsApp Cloud API access token; with the phone number id, app secret and verify token enables the WhatsApp channel (needs --listen and a public HTTPS URL)")
	l.str(&c.WhatsAppPhoneID, "whatsapp-phone-number-id", "", "id of the WhatsApp business phone number (shown in the Meta console; not the phone number itself)")
	l.secret(&c.WhatsAppAppSecret, "whatsapp-app-secret", "secret of the Meta app, to check WhatsApp webhook signatures")
	l.secret(&c.WhatsAppVerifyToken, "whatsapp-verify-token", "string you choose and enter in the Meta console when subscribing the WhatsApp webhook")
	l.str(&c.WhatsAppPath, "whatsapp-webhook-path", "/webhook/whatsapp", "path of the WhatsApp webhook on the HTTP server")
	l.str(&c.GraphAPI, "graph-api", "https://graph.facebook.com/v21.0", "Meta Graph API base URL (Messenger and WhatsApp)")
	l.boolean(&c.CLI, "cli", false, "enable the terminal channel (chat via stdin/stdout)")

	// Access control and limits
	l.str(&allowed, "allowed-users", "", "comma-separated user IDs (or channel:id) allowed to chat; empty allows everyone")
	l.str(&c.GroupReply, "group-reply", "mention", "reply in groups: mention (only when addressed) or all")
	l.str(&c.GroupContext, "group-context", "all", "what the bot remembers of groups: all (every message, so it knows who said what) or addressed (only messages for the bot)")
	l.integer(&c.RateLimit, "rate-limit", 20, "messages per user per minute; 0 = unlimited")
	l.integer(&c.MaxConcurrent, "max-concurrent", 4, "maximum simultaneous model runs")
	l.duration(&c.RequestTimeout, "request-timeout", 10*time.Minute, "maximum time to answer one message")
	l.integer(&c.MaxOpenSessions, "max-open-sessions", 64, "chat databases kept open at once")

	fs.Usage = usageFunc(fs, stderr, "jannyq [flags]")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if len(l.errs) > 0 {
		return nil, errors.Join(l.errs...)
	}

	c.TrustedProxies = splitList(proxies)
	c.WebAllowedOrigins = splitList(origins)
	for _, u := range strings.Split(allowed, ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.AllowedUsers = append(c.AllowedUsers, u)
		}
	}
	if systemPromptFile != "" {
		b, err := os.ReadFile(systemPromptFile)
		if err != nil {
			return nil, fmt.Errorf("--system-prompt-file: %w", err)
		}
		if c.SystemPrompt != "" {
			c.SystemPrompt += "\n\n"
		}
		c.SystemPrompt += strings.TrimSpace(string(b))
	}
	if c.LLMBaseURL == "" {
		switch c.LLMProvider {
		case "ollama":
			c.LLMBaseURL = "http://localhost:11434"
		case "openai":
			c.LLMBaseURL = "https://api.openai.com/v1"
		}
	}
	return c, c.validate()
}

func (c *Config) validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if strings.TrimSpace(c.Lang) == "" {
		bad("--lang must not be empty")
	}
	if c.LangMode != "default" && c.LangMode != "follow-user" {
		bad("--lang-mode must be default or follow-user")
	}
	if c.LLMProvider != "ollama" && c.LLMProvider != "openai" {
		bad("--llm-provider must be ollama or openai")
	}
	if c.LLMModel == "" {
		bad("--llm-model is required (JANNYQ_LLM_MODEL)")
	}
	if c.ContextSize < 0 {
		bad("--context-size must not be negative")
	}
	if c.CompactRatio <= 0 || c.CompactRatio > 1 {
		bad("--compact-ratio must be in (0, 1]")
	}
	if c.CompactAfter < 4 {
		bad("--compact-after must be at least 4")
	}
	if c.CompactKeep < 2 || c.CompactKeep >= c.CompactAfter {
		bad("--compact-keep must be at least 2 and less than --compact-after")
	}
	if c.MaxSteps < 1 {
		bad("--max-steps must be at least 1")
	}
	if c.GroupReply != "mention" && c.GroupReply != "all" {
		bad("--group-reply must be mention or all")
	}
	if c.GroupContext != "all" && c.GroupContext != "addressed" {
		bad("--group-context must be all or addressed")
	}
	if _, err := c.Location(); err != nil {
		bad("--timezone: %v", err)
	}
	if c.MaxConcurrent < 1 {
		bad("--max-concurrent must be at least 1")
	}
	switch c.RunCommand {
	case "off":
	case "sandbox":
		if c.SandboxURL == "" {
			bad("--sandbox-url is required with --run-command=sandbox")
		}
		if len(c.SandboxToken) < 16 {
			bad("--sandbox-token (JANNYQ_SANDBOX_TOKEN) must be set to a secret of at least 16 characters")
		}
	case "host":
		if !c.RunHostUnsafe {
			bad("--run-command=host runs untrusted commands directly on this machine; add --run-host-unsafe to confirm, or use sandbox")
		}
	default:
		bad("--run-command must be off, sandbox or host")
	}
	if c.RunRate < 0 {
		bad("--run-rate must not be negative")
	}
	for _, p := range c.TrustedProxies {
		if _, err := netip.ParsePrefix(p); err != nil {
			if _, err := netip.ParseAddr(p); err != nil {
				bad("--trusted-proxies: %q is not an IP address or CIDR range", p)
			}
		}
	}
	if c.HTTPRate < 0 {
		bad("--http-rate must not be negative")
	}
	if c.Web {
		if c.Listen == "" {
			bad("--web needs --listen (for example --listen :8080)")
		}
		if c.WebSecureCookies != "auto" && c.WebSecureCookies != "on" && c.WebSecureCookies != "off" {
			bad("--web-secure-cookies must be auto, on or off")
		}
		if c.WebMaxMessage < 1 || c.WebMaxMessage > 20000 {
			bad("--web-max-message must be between 1 and 20000")
		}
		if c.WebIPRate < 1 || c.WebSessionsPerHour < 1 {
			bad("--web-ip-rate and --web-sessions-per-hour must be at least 1")
		}
		if c.WebRunRate < -1 {
			bad("--web-run-rate must be -1, 0 or a positive number")
		}
		if c.WebSecret != "" && len(c.WebSecret) < 16 {
			bad("--web-secret must be at least 16 characters")
		}
		if strings.Contains(c.WebBasePath, "..") || strings.ContainsAny(c.WebBasePath, " ?#") {
			bad("--web-base-path must be a plain URL path such as / or /chat/")
		}
	}
	if c.RateLimit < 0 {
		bad("--rate-limit must not be negative")
	}
	c.validateMeta(bad)
	if (c.LineSecret == "") != (c.LineToken == "") {
		bad("the LINE channel needs both --line-channel-secret and --line-channel-token")
	}
	if c.LineSecret != "" {
		if c.Listen == "" {
			bad("the LINE channel needs --listen (LINE calls a webhook on this server; put it behind HTTPS)")
		}
		if !strings.HasPrefix(c.LinePath, "/") || strings.ContainsAny(c.LinePath, " ?#") || strings.Contains(c.LinePath, "..") {
			bad("--line-webhook-path must be a plain URL path such as /webhook/line")
		}
		if c.Web && strings.TrimSuffix(c.WebBasePath, "/") != "" && strings.HasPrefix(c.LinePath, c.WebBasePath) {
			bad("--line-webhook-path must not lie under --web-base-path")
		}
	}
	switch c.Vision {
	case "auto", "on", "off":
	default:
		bad("--vision must be auto, on or off")
	}
	switch c.PDFEngine {
	case "auto", "native":
	case "sandbox":
		if c.SandboxURL == "" || len(c.SandboxToken) < 16 {
			bad("--pdf-engine=sandbox needs --sandbox-url and --sandbox-token (at least 16 characters)")
		}
	default:
		bad("--pdf-engine must be auto, sandbox or native")
	}
	if c.AttachMaxMB < 1 || c.AttachMaxMB > 200 {
		bad("--attach-max-mb must be between 1 and 200")
	}
	if c.AttachPerMessage < 1 || c.AttachRate < 0 || c.AttachChatMB < 1 {
		bad("--attach-per-message and --attach-chat-mb must be at least 1, --attach-rate not negative")
	}
	if c.ImageMaxEdge < 64 || c.ImageMaxEdge > 8192 {
		bad("--image-max-edge must be between 64 and 8192")
	}
	if c.ImageMessages < 1 || c.VisionPages < 0 || c.OCRMaxPages < 0 || c.PDFMaxPages < 1 || c.AttachInlineChars < 100 {
		bad("--image-messages and --pdf-max-pages must be at least 1, --attach-inline-chars at least 100")
	}
	if l := strings.TrimSpace(c.OCRLangs); l != "auto" && l != "off" && l != "" {
		for _, r := range l {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '+') {
				bad("--ocr-langs must be like eng+tha, auto or off")
				break
			}
		}
	}
	if c.KnowledgeDir != "" {
		switch c.EmbedProvider {
		case "", "ollama", "openai":
		default:
			bad("--embed-provider must be ollama or openai")
		}
		if c.KnowledgeChunkChars < 200 || c.KnowledgeChunkChars > 20000 {
			bad("--knowledge-chunk-chars must be between 200 and 20000")
		}
		if c.KnowledgeOverlap < 0 || c.KnowledgeOverlap > c.KnowledgeChunkChars/2 {
			bad("--knowledge-overlap must be between 0 and half of --knowledge-chunk-chars")
		}
		if c.KnowledgeResults < 1 || c.KnowledgeResults > 10 {
			bad("--knowledge-results must be between 1 and 10")
		}
		if c.KnowledgeMinSim < 0 || c.KnowledgeMinSim >= 1 {
			bad("--knowledge-min-similarity must be in [0, 1)")
		}
		if c.KnowledgeMaxFileMB < 1 || c.KnowledgePDFPages < 1 || c.KnowledgeOCRPages < 0 {
			bad("--knowledge-max-file-mb and --knowledge-pdf-max-pages must be at least 1, --knowledge-ocr-pages not negative")
		}
		if c.KnowledgeInterval < time.Second {
			bad("--knowledge-interval must be at least 1s")
		}
	}
	if c.WebMaxUploadMB < 1 || c.WebMaxFiles < 1 {
		bad("--web-max-upload-mb and --web-max-files must be at least 1")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		bad("--log-level must be debug, info, warn or error")
	}
	return errors.Join(errs...)
}

func splitList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// usageFunc prints every flag with its environment variable (which is part
// of each flag's usage text).
func usageFunc(fs *flag.FlagSet, w io.Writer, synopsis string) func() {
	return func() {
		fmt.Fprintln(w, "Usage:", synopsis)
		fmt.Fprintln(w, "\nEvery flag can also be set with the environment variable shown in brackets.")
		fmt.Fprintln(w, "Flags override the environment. Prefer environment variables for secrets.")
		fmt.Fprintln(w)
		var names []string
		fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
		sort.Strings(names)
		for _, n := range names {
			f := fs.Lookup(n)
			def := ""
			if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
				def = " (default " + f.DefValue + ")"
			}
			fmt.Fprintf(w, "  --%s\n        %s%s\n", f.Name, f.Usage, def)
		}
	}
}

// Location returns the time zone for message timestamps.
func (c *Config) Location() (*time.Location, error) {
	name := strings.TrimSpace(c.Timezone)
	if name == "" && strings.HasPrefix(strings.ToLower(c.Lang), "th") {
		name = "Asia/Bangkok"
	}
	if name == "" {
		return time.Local, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q (use an IANA name such as Asia/Bangkok or Europe/London)", name)
	}
	return loc, nil
}

// OCRLanguages returns the Tesseract languages to use, "" when OCR is off.
func (c *Config) OCRLanguages() string {
	switch l := strings.TrimSpace(c.OCRLangs); l {
	case "off":
		return ""
	case "", "auto":
		if strings.HasPrefix(strings.ToLower(c.Lang), "th") {
			return "eng+tha"
		}
		return "eng"
	default:
		return l
	}
}

// validateMeta checks the Messenger and WhatsApp settings.
func (c *Config) validateMeta(bad func(format string, a ...any)) {
	messenger := c.MessengerToken != "" || c.MessengerAppSecret != "" || c.MessengerVerifyToken != ""
	whatsapp := c.WhatsAppToken != "" || c.WhatsAppPhoneID != "" || c.WhatsAppAppSecret != "" || c.WhatsAppVerifyToken != ""
	if messenger && (c.MessengerToken == "" || c.MessengerAppSecret == "" || c.MessengerVerifyToken == "") {
		bad("the Messenger channel needs --messenger-page-token, --messenger-app-secret and --messenger-verify-token")
	}
	if whatsapp && (c.WhatsAppToken == "" || c.WhatsAppPhoneID == "" || c.WhatsAppAppSecret == "" || c.WhatsAppVerifyToken == "") {
		bad("the WhatsApp channel needs --whatsapp-token, --whatsapp-phone-number-id, --whatsapp-app-secret and --whatsapp-verify-token")
	}
	if !messenger && !whatsapp {
		return
	}
	if c.Listen == "" {
		bad("the Messenger and WhatsApp channels need --listen (Meta calls a webhook on this server; put it behind HTTPS)")
	}
	paths := map[string]string{}
	check := func(name, p string, on bool) {
		if !on {
			return
		}
		if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, " ?#") || strings.Contains(p, "..") {
			bad("%s must be a plain URL path such as /webhook/messenger", name)
			return
		}
		if other, dup := paths[p]; dup {
			bad("%s and %s use the same path %s", name, other, p)
		}
		paths[p] = name
		if c.Web && strings.TrimSuffix(c.WebBasePath, "/") != "" && strings.HasPrefix(p, c.WebBasePath) {
			bad("%s must not lie under --web-base-path", name)
		}
	}
	check("--messenger-webhook-path", c.MessengerPath, messenger)
	check("--whatsapp-webhook-path", c.WhatsAppPath, whatsapp)
	check("--line-webhook-path", c.LinePath, c.LineSecret != "")
}
