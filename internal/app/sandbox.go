package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/authapon/jannyq/internal/audit"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/sandbox"
)

// execConfig converts the sandbox command-line settings.
func execConfig(c *config.SandboxConfig) sandbox.ExecConfig {
	return sandbox.ExecConfig{
		WorkDir:        c.WorkDir,
		Shell:          c.Shell,
		Network:        c.Network == "on",
		DefaultTimeout: c.DefaultTimeout,
		MaxTimeout:     c.MaxTimeout,
		MaxOutputBytes: c.MaxOutputKB << 10,
		CPUSeconds:     c.CPUSeconds,
		MaxFileBytes:   int64(c.MaxFileMB) << 20,
		MaxOpenFiles:   c.OpenFiles,
		UploadMaxBytes: int64(c.UploadMB) << 20,
		MaxProcesses:   c.MaxProcs,
		WorkspaceQuota: int64(c.QuotaMB) << 20,
		MaxConcurrent:  c.MaxConcurrent,
		IdleTTL:        c.IdleTTL,
		UIDBase:        c.UIDBase,
		UIDCount:       c.UIDCount,
		AllowRoot:      c.AllowRoot,
	}
}

// RunSandbox serves the command executor (`jannyq sandbox`) until ctx ends.
func RunSandbox(ctx context.Context, cfg *config.SandboxConfig, version string, log *slog.Logger) error {
	ex, err := sandbox.NewExecutor(execConfig(cfg), log)
	if err != nil {
		return err
	}
	srv, err := sandbox.NewServer(ex, cfg.Token, cfg.MaxTimeout, log)
	if err != nil {
		return err
	}
	if os.Geteuid() == 0 && cfg.UIDCount > 0 {
		log.Info("each workspace runs as its own unprivileged user", "uid_range", fmt.Sprintf("%d-%d", cfg.UIDBase, cfg.UIDBase+cfg.UIDCount-1))
	} else {
		log.Warn("commands run as the executor's own user: workspaces are not isolated from each other")
	}
	log.Info("sandbox starting", "version", version, "workdir", cfg.WorkDir, "network", cfg.Network,
		"max_timeout", cfg.MaxTimeout, "quota_mb", cfg.QuotaMB, "max_concurrent", cfg.MaxConcurrent)

	go ex.Janitor(ctx)
	return srv.Serve(ctx, cfg.Listen)
}

// commandRunner is what the bot uses to run commands, plus its description.
type commandRunner struct {
	runner sandbox.Runner
	info   *sandbox.Info
	audit  *audit.Logger
}

// newCommandRunner builds the runner for --run-command, or returns nil when
// command execution is off.
func newCommandRunner(ctx context.Context, cfg *config.Config, log *slog.Logger) (*commandRunner, error) {
	if cfg.RunCommand == "off" {
		return nil, nil
	}
	path := cfg.AuditLog
	if path == "" {
		path = filepath.Join(cfg.DataDir, "audit", "commands.jsonl")
	}
	al, err := audit.Open(path, 0)
	if err != nil {
		return nil, fmt.Errorf("open the command audit log: %w", err)
	}
	cr := &commandRunner{audit: al}

	switch cfg.RunCommand {
	case "sandbox":
		client := &sandbox.Client{BaseURL: cfg.SandboxURL, Token: cfg.SandboxToken, HTTP: &http.Client{}}
		cr.runner = client
		cr.info = waitForInfo(ctx, client, log)
	case "host":
		log.Warn("run_command executes untrusted commands directly on this machine (--run-command=host); " +
			"use the sandbox container for anything but a private test")
		ex, err := sandbox.NewExecutor(sandbox.ExecConfig{
			WorkDir:   filepath.Join(cfg.DataDir, "workspaces"),
			AllowRoot: true,
			Network:   true,
		}, log)
		if err != nil {
			al.Close()
			return nil, err
		}
		go ex.Janitor(ctx)
		info := ex.Info()
		cr.runner, cr.info = ex, &info
	}
	return cr, nil
}

// waitForInfo asks the sandbox for its capabilities, giving it a few seconds
// to come up. The bot works without them; only the tool description is less precise.
func waitForInfo(ctx context.Context, c *sandbox.Client, log *slog.Logger) *sandbox.Info {
	deadline := time.Now().Add(15 * time.Second)
	for {
		info, err := c.Info(ctx)
		if err == nil {
			return info
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			log.Warn("the sandbox did not answer; run_command will fail until it is reachable", "err", err)
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
	}
}
