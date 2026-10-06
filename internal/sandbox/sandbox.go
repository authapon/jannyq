// Package sandbox runs shell commands on behalf of the model under strict
// limits. The Executor does the work (inside the separate sandbox container
// in a normal deployment); Server and Client expose it over HTTP so the bot
// process, which holds the secrets, never executes untrusted commands itself.
package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"regexp"
	"time"
)

// Errors returned by runners.
var (
	ErrBusy             = errors.New("sandbox: busy, try again shortly")
	ErrInvalidWorkspace = errors.New("sandbox: invalid workspace id")
	ErrEmptyCommand     = errors.New("sandbox: empty command")
	ErrCommandTooLong   = errors.New("sandbox: command too long")
	ErrUnavailable      = errors.New("sandbox: service unavailable")
	ErrBadPath          = errors.New("sandbox: invalid file path")
	ErrNotFound         = errors.New("sandbox: file not found")
	ErrTooLarge         = errors.New("sandbox: file too large")
	ErrQuota            = errors.New("sandbox: workspace is full")
)

// MaxCommandBytes bounds the size of one command line.
const MaxCommandBytes = 32 << 10

// Request asks to run one shell command in a workspace.
type Request struct {
	// Workspace identifies the persistent per-chat directory. Use WorkspaceID.
	Workspace string `json:"workspace"`
	Command   string `json:"command"`
	// TimeoutSeconds and MaxOutputBytes may only lower the server's limits.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	MaxOutputBytes int `json:"max_output_bytes,omitempty"`
}

// Result is the outcome of a command. A non-zero exit code is not an error.
type Result struct {
	ExitCode int    `json:"exit_code"`
	Signal   string `json:"signal,omitempty"`
	// Output is stdout and stderr interleaved; long output keeps its head and
	// tail, with OmittedBytes counting what was dropped from the middle.
	Output       string `json:"output"`
	OmittedBytes int64  `json:"omitted_bytes,omitempty"`
	TimedOut     bool   `json:"timed_out,omitempty"`
	// WritesDisabled is set when the workspace is over quota: the command ran
	// with file writes disabled so it can still delete files.
	WritesDisabled bool  `json:"writes_disabled,omitempty"`
	DurationMS     int64 `json:"duration_ms"`
}

// Info describes the sandbox so the model can be told what it can do.
type Info struct {
	Network        bool     `json:"network"`
	DefaultTimeout int      `json:"default_timeout_seconds"`
	MaxTimeout     int      `json:"max_timeout_seconds"`
	MaxOutputBytes int      `json:"max_output_bytes"`
	QuotaBytes     int64    `json:"workspace_quota_bytes"`
	Tools          []string `json:"tools,omitempty"`
	// Files is true when the sandbox accepts file uploads and downloads.
	Files bool `json:"files,omitempty"`
}

// Files moves files in and out of a workspace. Paths are relative to the
// workspace and may not leave it.
type Files interface {
	// Put stores up to maxBytes read from r at path, creating directories.
	Put(ctx context.Context, workspace, path string, r io.Reader, maxBytes int64) error
	// Get returns the file at path; larger files fail with ErrTooLarge.
	Get(ctx context.Context, workspace, path string, maxBytes int64) ([]byte, error)
	// Remove deletes the file at path.
	Remove(ctx context.Context, workspace, path string) error
}

// Runner executes commands. Executor (in process) and Client (remote) both
// implement it.
type Runner interface {
	Run(ctx context.Context, req Request) (*Result, error)
	// Reset deletes the workspace's files.
	Reset(ctx context.Context, workspace string) error
}

// WorkspaceID derives the workspace identifier for a chat key such as
// "telegram:12345". The sandbox only ever sees this hash, never the chat ID.
func WorkspaceID(chatKey string) string {
	sum := sha256.Sum256([]byte("jannyq-workspace\x00" + chatKey))
	return hex.EncodeToString(sum[:16])
}

var workspaceRe = regexp.MustCompile(`^[a-f0-9]{16,64}$`)

func validWorkspace(id string) bool { return workspaceRe.MatchString(id) }

func seconds(d time.Duration) int { return int(d / time.Second) }
