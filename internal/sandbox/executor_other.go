//go:build !unix

package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ExecConfig is unused on this platform.
type ExecConfig struct {
	WorkDir                      string
	Network                      bool
	DefaultTimeout, MaxTimeout   time.Duration
	MaxOutputBytes, CPUSeconds   int
	MaxFileBytes, WorkspaceQuota int64
	MaxOpenFiles, MaxConcurrent  int
	MaxProcesses                 int
	TmpDir, HelperPath           string
	IdleTTL, JanitorEvery        time.Duration
	UIDBase, UIDCount            int
	AllowRoot                    bool
	Shell                        string
	ExtraEnv, ToolCandidates     []string
}

// Executor is not available on this platform.
type Executor struct{}

// NewExecutor always fails: command execution needs a Unix system.
func NewExecutor(ExecConfig, *slog.Logger) (*Executor, error) {
	return nil, errors.New("sandbox: command execution is only supported on Unix systems")
}

func (*Executor) Run(context.Context, Request) (*Result, error) {
	return nil, errors.New("unsupported")
}
func (*Executor) Reset(context.Context, string) error { return errors.New("unsupported") }
func (*Executor) Info() Info                          { return Info{} }
func (*Executor) Janitor(context.Context)             {}
