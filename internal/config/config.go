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
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Config holds all settings.
type Config struct {
	// General
	Lang     string
	LangMode string
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

	// Channels
	TelegramToken string
	TelegramAPI   string
	CLI           bool

	// Access control and limits
	AllowedUsers    []string
	GroupReply      string
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

	var allowed, systemPromptFile string

	// General
	l.str(&c.Lang, "lang", "en", "main language for messages and replies (en, th, ...)")
	l.str(&c.LangMode, "lang-mode", "follow-user", "reply language: follow-user (mirror the user) or default (always --lang)")
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

	// Channels
	l.secret(&c.TelegramToken, "telegram-token", "Telegram bot token; enables the Telegram channel")
	l.str(&c.TelegramAPI, "telegram-api", "https://api.telegram.org", "Telegram Bot API base URL")
	l.boolean(&c.CLI, "cli", false, "enable the terminal channel (chat via stdin/stdout)")

	// Access control and limits
	l.str(&allowed, "allowed-users", "", "comma-separated user IDs (or channel:id) allowed to chat; empty allows everyone")
	l.str(&c.GroupReply, "group-reply", "mention", "reply in groups: mention (only when addressed) or all")
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
	if c.RateLimit < 0 {
		bad("--rate-limit must not be negative")
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		bad("--log-level must be debug, info, warn or error")
	}
	return errors.Join(errs...)
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
