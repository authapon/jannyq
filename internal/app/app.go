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
		tools.Register(&tool.RunCommand{
			Runner:     runner.runner,
			Info:       runner.info,
			Limiter:    ratelimit.New(cfg.RunRate, time.Minute),
			Audit:      runner.audit,
			Log:        log,
			SkillsPath: skillsPath,
		})
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
	acfg := agent.Config{
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
		RateLimit:      cfg.RateLimit,
		MaxConcurrent:  cfg.MaxConcurrent,
		RequestTimeout: cfg.RequestTimeout,
	}, sessions, ag, tr, log)
	if runner != nil {
		rt.SetWorkspaceReset(runner.runner.Reset)
	}

	var channels []channel.Channel
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
		return errors.New("no channel enabled: set JANNYQ_TELEGRAM_TOKEN or use --cli")
	}

	log.Info("jannyq starting",
		"version", version, "model", cfg.LLMModel, "provider", cfg.LLMProvider,
		"context_size", cfg.ContextSize, "lang", cfg.Lang,
		"tools", tools.Len(), "run_command", cfg.RunCommand, "channels", len(channels), "data_dir", cfg.DataDir)

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errc := make(chan error, len(channels)+1)
	if cfg.Listen != "" {
		srv := server.New(cfg.Listen, version, log)
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
	if runErr == nil {
		select {
		case runErr = <-errc:
		default:
		}
	}
	return runErr
}
