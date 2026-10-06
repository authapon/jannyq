//go:build !unix

package sandbox

import (
	"context"
	"errors"
	"io"
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
	UploadMaxBytes               int64
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

func (*Executor) Put(context.Context, string, string, io.Reader, int64) error {
	return errors.New("unsupported")
}

func (*Executor) Get(context.Context, string, string, int64) ([]byte, error) {
	return nil, errors.New("unsupported")
}

func (*Executor) Remove(context.Context, string, string) error { return errors.New("unsupported") }
