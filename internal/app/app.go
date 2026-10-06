// Package app wires the configuration into a running bot.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/authapon/jannyq/internal/agent"
	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/channel/cli"
	"github.com/authapon/jannyq/internal/channel/telegram"
	"github.com/authapon/jannyq/internal/channel/web"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/i18n"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/ratelimit"
	"github.com/authapon/jannyq/internal/router"
	"github.com/authapon/jannyq/internal/server"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/skill"
	"github.com/authapon/jannyq/internal/tool"
)

// NewLogger builds the process logger from the configuration.
func NewLogger(cfg *config.Config, w io.Writer) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(strings.ToLower(cfg.LogLevel)))
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// NewProvider creates the model client for the configured API.
func NewProvider(cfg *config.Config) (llm.Provider, error) {
	client := &http.Client{Timeout: cfg.LLMTimeout}
	switch cfg.LLMProvider {
	case "ollama":
		return &llm.Ollama{BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Client: client}, nil
	case "openai":
		return &llm.OpenAI{BaseURL: cfg.LLMBaseURL, APIKey: cfg.LLMAPIKey, Client: client}, nil
	}
	return nil, fmt.Errorf("unknown llm provider %q", cfg.LLMProvider)
}

// NewTools registers the tools enabled by the configuration.
func NewTools(cfg *config.Config, version string) *tool.Registry {
	reg := tool.NewRegistry()
	if cfg.SearxngURL != "" {
		reg.Register(&tool.WebSearch{
			BaseURL:    cfg.SearxngURL,
			Client:     &http.Client{Timeout: cfg.FetchTimeout},
			MaxResults: cfg.SearchMaxResults,
		})
	}
	reg.Register(&tool.WebFetch{
		Client:    tool.NewSafeClient(cfg.FetchAllowPrivate, cfg.FetchTimeout),
		MaxBytes:  cfg.FetchMaxBytes,
		MaxChars:  cfg.ToolMaxOutput,
		UserAgent: "Mozilla/5.0 (compatible; jannyq/" + version + "; +https://github.com/authapon/jannyq)",
	})
	return reg
}

// Run starts the configured channels and serves until ctx is cancelled or a
// channel fails.
func Run(ctx context.Context, cfg *config.Config, version string, log *slog.Logger) error {
	provider, err := NewProvider(cfg)
	if err != nil {
		return err
	}
	tools := NewTools(cfg, version)
	if cfg.FetchAllowPrivate {
		log.Warn("web_fetch may reach private addresses (--fetch-allow-private)")
	}

	runner, err := newCommandRunner(ctx, cfg, log)
	if err != nil {
		return err
	}
	if runner != nil {
		defer runner.audit.Close()
		skillsPath := ""
		if cfg.SkillsDir != "" && cfg.RunCommand == "sandbox" {
			skillsPath = cfg.SkillsSandboxPath
		}
		rc := &tool.RunCommand{
			Runner:     runner.runner,
			Info:       runner.info,
			Limiter:    ratelimit.New(cfg.RunRate, time.Minute),
			Audit:      runner.audit,
			Log:        log,
			SkillsPath: skillsPath,
		}
		if cfg.Web {
			// Web visitors are anonymous, so their commands get a tighter,
			// per-address quota (or none at all).
			switch {
			case cfg.WebRunRate == 0:
				rc.DisabledChannels = map[string]bool{"web": true}
			case cfg.WebRunRate > 0:
				rc.ChannelLimiters = map[string]*ratelimit.Limiter{"web": ratelimit.New(cfg.WebRunRate, time.Minute)}
			}
		}
		tools.Register(rc)
	}
	var skills *skill.Store
	if cfg.SkillsDir != "" {
		skills, err = skill.NewStore(cfg.SkillsDir, cfg.SkillsSandboxPath, log)
		if err != nil {
			return fmt.Errorf("skills: %w", err)
		}
		tools.Register(&tool.LoadSkill{Store: skills})
		log.Info("skills loaded", "dir", cfg.SkillsDir, "count", len(skills.Summaries()))
	}

	var temp *float64
	if cfg.Temperature >= 0 {
		t := cfg.Temperature
		temp = &t
	}
	loc, err := cfg.Location()
	if err != nil {
		return err
	}
	acfg := agent.Config{
		Location:      loc,
		Model:         cfg.LLMModel,
		ContextSize:   cfg.ContextSize,
		Temperature:   temp,
		MaxSteps:      cfg.MaxSteps,
		Lang:          cfg.Lang,
		LangMode:      cfg.LangMode,
		BotName:       cfg.BotName,
		ExtraPrompt:   cfg.SystemPrompt,
		CompactAfter:  cfg.CompactAfter,
		CompactRatio:  cfg.CompactRatio,
		CompactKeep:   cfg.CompactKeep,
		ToolMaxOutput: cfg.ToolMaxOutput,
	}
	if skills != nil {
		acfg.Skills = skills
	}
	att := newAttachments(ctx, cfg, provider, runner, log)
	if att != nil {
		tools.Register(tool.ReadAttachment{})
		tools.Register(tool.SearchAttachment{})
		acfg.Attachments = true
		acfg.Vision = att.vision
		acfg.ImageMessages = cfg.ImageMessages
		acfg.InlineChars = cfg.AttachInlineChars
		acfg.Inbox = att.files != nil
	}
	ag := agent.New(acfg, provider, tools, log)

	sessionsDir := filepath.Join(cfg.DataDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o750); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	sessions := session.NewManager(sessionsDir, cfg.MaxOpenSessions, 4)
	defer sessions.Close()

	tr := i18n.New(cfg.Lang)
	rt := router.New(router.Config{
		AllowedUsers:   cfg.AllowedUsers,
		GroupReply:     cfg.GroupReply,
		GroupContext:   cfg.GroupContext,
		CompactAfter:   cfg.CompactAfter,
		RateLimit:      cfg.RateLimit,
		MaxConcurrent:  cfg.MaxConcurrent,
		RequestTimeout: cfg.RequestTimeout,
	}, sessions, ag, tr, log)
	if runner != nil {
		rt.SetWorkspaceReset(runner.runner.Reset)
	}
	if att != nil {
		rt.SetAttachments(att.routerConfig(cfg))
	}

	var srv *server.Server
	if cfg.Listen != "" {
		proxies, err := server.ParseProxies(cfg.TrustedProxies)
		if err != nil {
			return fmt.Errorf("trusted proxies: %w", err)
		}
		srv = server.New(server.Options{
			Addr: cfg.Listen, Version: version, TrustedProxies: proxies, RatePerMinute: cfg.HTTPRate, Log: log,
		})
	}

	var channels []channel.Channel
	if cfg.Web {
		secret, err := webSecret(cfg)
		if err != nil {
			return err
		}
		title := cfg.WebTitle
		if title == "" {
			title = cfg.BotName
		}
		wc, err := web.New(web.Config{
			BasePath:           cfg.WebBasePath,
			Title:              title,
			Lang:               tr.Lang(),
			AccessCode:         cfg.WebAccessCode,
			Secret:             secret,
			MaxMessage:         cfg.WebMaxMessage,
			IPRate:             cfg.WebIPRate,
			NewSessionsPerHour: cfg.WebSessionsPerHour,
			SecureCookies:      cfg.WebSecureCookies,
			AllowedOrigins:     cfg.WebAllowedOrigins,
			Strings:            tr.Prefixed("web_"),
			History:            webHistory{sessions},
			Attachments:        att != nil,
			MaxUploadBytes:     int64(min(cfg.WebMaxUploadMB, cfg.AttachMaxMB)) << 20,
			MaxFiles:           cfg.WebMaxFiles,
		}, srv, log)
		if err != nil {
			return err
		}
		channels = append(channels, wc)
		if cfg.WebAccessCode == "" {
			log.Warn("the web chat is open to anyone who can reach it (no --web-access-code)")
		}
	}
	if cfg.TelegramToken != "" {
		channels = append(channels, telegram.New(telegram.Config{
			Token:   cfg.TelegramToken,
			APIBase: cfg.TelegramAPI,
		}, log))
	}
	if cfg.CLI {
		channels = append(channels, cli.New())
	}
	if len(channels) == 0 {
		return errors.New("no channel enabled: set JANNYQ_TELEGRAM_TOKEN, use --web or use --cli")
	}

	log.Info("jannyq starting",
		"version", version, "model", cfg.LLMModel, "provider", cfg.LLMProvider,
		"context_size", cfg.ContextSize, "lang", cfg.Lang, "timezone", loc.String(), "group_context", cfg.GroupContext,
		"tools", tools.Len(), "run_command", cfg.RunCommand, "channels", len(channels), "data_dir", cfg.DataDir)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errc := make(chan error, len(channels)+1)
	if srv != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := srv.Run(runCtx); err != nil {
				errc <- fmt.Errorf("http server: %w", err)
			}
		}()
	}
	active := len(channels)
	var mu sync.Mutex
	for _, ch := range channels {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := ch.Run(runCtx, rt.Handle)
			if err != nil && runCtx.Err() == nil {
				errc <- fmt.Errorf("%s channel: %w", ch.Name(), err)
				return
			}
			// A channel that ends on its own (CLI at end of input) stops the
			// bot once no other channel is left.
			mu.Lock()
			active--
			last := active == 0
			mu.Unlock()
			if last {
				cancel()
			}
		}()
	}

	var runErr error
	select {
	case runErr = <-errc:
		cancel()
	case <-runCtx.Done():
	}
	wg.Wait()
	rt.Wait()
	if runErr == nil {
		select {
		case runErr = <-errc:
		default:
		}
	}
	return runErr
}
