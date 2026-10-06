//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ExecConfig configures an Executor. Zero values select safe defaults.
type ExecConfig struct {
	WorkDir string // base directory holding one subdirectory per workspace
	Shell   string // default /bin/sh
	// Network only describes whether the surrounding container has network
	// access (reported to the model); it is enforced by the deployment.
	Network bool

	DefaultTimeout time.Duration // default 30s
	MaxTimeout     time.Duration // default 120s
	MaxOutputBytes int           // default 64 KiB
	CPUSeconds     int           // RLIMIT_CPU per command; default = MaxTimeout
	MaxFileBytes   int64         // largest single file a command may write; default 100 MiB
	MaxOpenFiles   int           // default 256
	MaxProcesses   int           // RLIMIT_NPROC per command; only meaningful with per-workspace users; default 128
	WorkspaceQuota int64         // default 256 MiB
	MaxConcurrent  int           // simultaneous commands; default 4
	IdleTTL        time.Duration // delete workspaces unused for this long; default 7 days
	JanitorEvery   time.Duration // default 10 minutes

	// Each workspace runs as its own unprivileged user taken from
	// [UIDBase, UIDBase+UIDCount). This needs the executor to run as root with
	// CAP_SETUID, CAP_SETGID, CAP_CHOWN and CAP_DAC_OVERRIDE; it keeps
	// workspaces from reading, modifying or signalling each other, and keeps
	// commands away from the executor's own memory and secrets.
	UIDBase, UIDCount int
	// TmpDir is the shared temporary directory whose files are deleted when a
	// workspace's user is recycled; default /tmp.
	TmpDir string
	// AllowRoot permits running commands as the executor's own user when no
	// per-workspace users are available. Dangerous when that user is root.
	AllowRoot bool

	// HelperPath is the program that applies resource limits and starts the
	// shell (see HelperMain); default: this executable. Commands' users must
	// be able to execute it.
	HelperPath string

	ExtraEnv       []string // additional KEY=VALUE for commands
	ToolCandidates []string // programs to look for when reporting Info
}

func (c *ExecConfig) defaults() error {
	if c.Shell == "" {
		c.Shell = "/bin/sh"
	}
	if c.DefaultTimeout <= 0 {
		c.DefaultTimeout = 30 * time.Second
	}
	if c.MaxTimeout <= 0 {
		c.MaxTimeout = 120 * time.Second
	}
	if c.DefaultTimeout > c.MaxTimeout {
		c.DefaultTimeout = c.MaxTimeout
	}
	if c.MaxOutputBytes <= 0 {
		c.MaxOutputBytes = 64 << 10
	}
	if c.CPUSeconds <= 0 {
		c.CPUSeconds = seconds(c.MaxTimeout)
	}
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = 100 << 20
	}
	if c.MaxOpenFiles <= 0 {
		c.MaxOpenFiles = 256
	}
	if c.MaxProcesses <= 0 {
		c.MaxProcesses = 128
	}
	if c.TmpDir == "" {
		c.TmpDir = os.TempDir()
	}
	if c.WorkspaceQuota <= 0 {
		c.WorkspaceQuota = 256 << 20
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 4
	}
	if c.IdleTTL <= 0 {
		c.IdleTTL = 7 * 24 * time.Hour
	}
	if c.JanitorEvery <= 0 {
		c.JanitorEvery = 10 * time.Minute
	}
	if c.HelperPath == "" {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("sandbox: cannot locate the executor binary: %w", err)
		}
		c.HelperPath = self
	}
	if len(c.ToolCandidates) == 0 {
		c.ToolCandidates = []string{"python3", "node", "curl", "wget", "jq", "git", "bc", "awk", "sed",
			"grep", "tar", "zip", "unzip", "sqlite3", "openssl", "tree", "file", "ffmpeg", "convert", "pdftotext"}
	}
	return nil
}

// Executor runs commands in per-workspace directories.
type Executor struct {
	cfg ExecConfig
	log *slog.Logger

	isolate bool // per-workspace users in use

	slots chan struct{} // global concurrency

	mu     sync.Mutex
	busy   map[string]bool   // workspaces with a command (or reset) in progress
	owners map[uint32]string // uid -> workspace
}

// NewExecutor prepares the work directory and returns an Executor.
func NewExecutor(cfg ExecConfig, log *slog.Logger) (*Executor, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.WorkDir == "" {
		return nil, errors.New("sandbox: work directory is required")
	}
	if err := os.MkdirAll(cfg.WorkDir, 0o711); err != nil {
		return nil, err
	}
	// Workspace users must be able to traverse the base directory.
	if err := os.Chmod(cfg.WorkDir, 0o711); err != nil {
		return nil, err
	}
	e := &Executor{
		cfg:    cfg,
		log:    log,
		slots:  make(chan struct{}, cfg.MaxConcurrent),
		busy:   map[string]bool{},
		owners: map[uint32]string{},
	}
	root := os.Geteuid() == 0
	switch {
	case cfg.UIDCount > 0 && root:
		e.isolate = true
	case cfg.UIDCount > 0:
		log.Warn("not running as root: per-workspace users are unavailable, commands run as the executor's user")
	}
	if root && !e.isolate && !cfg.AllowRoot {
		return nil, errors.New("sandbox: refusing to run commands as root; configure a uid range (run-as-uid-base/count) or set allow-root")
	}
	if e.isolate {
		e.scanOwners()
	}
	return e, nil
}

func (e *Executor) scanOwners() {
	entries, _ := os.ReadDir(e.cfg.WorkDir)
	for _, ent := range entries {
		if !ent.IsDir() || !validWorkspace(ent.Name()) {
			continue
		}
		if uid, ok := ownerOf(filepath.Join(e.cfg.WorkDir, ent.Name())); ok && e.inRange(uid) {
			e.owners[uid] = ent.Name()
		}
	}
}

func (e *Executor) inRange(uid uint32) bool {
	return int64(uid) >= int64(e.cfg.UIDBase) && int64(uid) < int64(e.cfg.UIDBase)+int64(e.cfg.UIDCount)
}

func ownerOf(path string) (uint32, bool) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Uid, true
}

// Info reports the sandbox's capabilities.
func (e *Executor) Info() Info {
	info := Info{
		Network:        e.cfg.Network,
		DefaultTimeout: seconds(e.cfg.DefaultTimeout),
		MaxTimeout:     seconds(e.cfg.MaxTimeout),
		MaxOutputBytes: e.cfg.MaxOutputBytes,
		QuotaBytes:     e.cfg.WorkspaceQuota,
	}
	for _, t := range e.cfg.ToolCandidates {
		if _, err := exec.LookPath(t); err == nil {
			info.Tools = append(info.Tools, t)
		}
	}
	return info
}

func (e *Executor) lock(ws string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy[ws] {
		return false
	}
	e.busy[ws] = true
	return true
}

func (e *Executor) unlock(ws string) {
	e.mu.Lock()
	delete(e.busy, ws)
	e.mu.Unlock()
}

// prepare returns the workspace directory and the user to run as (-1 when
// commands run as the executor's own user), creating the workspace if needed.
func (e *Executor) prepare(ws string) (dir string, uid int, err error) {
	dir = filepath.Join(e.cfg.WorkDir, ws)
	uid = -1
	fi, statErr := os.Lstat(dir)
	exists := statErr == nil
	if exists && !fi.IsDir() {
		return "", -1, fmt.Errorf("sandbox: workspace path is not a directory")
	}
	if e.isolate {
		cur, ok := uint32(0), false
		if exists {
			cur, ok = ownerOf(dir)
		}
		switch {
		case ok && e.inRange(cur):
			uid = int(cur)
			e.mu.Lock()
			e.owners[cur] = ws
			e.mu.Unlock()
		default:
			u, err := e.allocUID(ws)
			if err != nil {
				return "", -1, err
			}
			uid = int(u)
		}
	}
	if !exists {
		if e.isolate {
			e.cleanTmp(uint32(uid)) // leftovers of a previous owner of this user id
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			return "", -1, err
		}
	}
	if e.isolate {
		if cur, _ := ownerOf(dir); int(cur) != uid {
			if err := chownTree(dir, uid); err != nil {
				return "", -1, fmt.Errorf("sandbox: preparing workspace: %w", err)
			}
		}
	}
	tmp := filepath.Join(dir, ".tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return "", -1, err
	}
	if e.isolate {
		if err := os.Lchown(tmp, uid, uid); err != nil {
			return "", -1, err
		}
	}
	return dir, uid, nil
}

// allocUID picks a free user for a new workspace, starting at a hash-derived
// slot so that the choice is stable and spread out.
func (e *Executor) allocUID(ws string) (uint32, error) {
	h := fnv.New32a()
	h.Write([]byte(ws))
	start := int(h.Sum32() % uint32(e.cfg.UIDCount))
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := 0; i < e.cfg.UIDCount; i++ {
		uid := uint32(e.cfg.UIDBase + (start+i)%e.cfg.UIDCount)
		if owner, taken := e.owners[uid]; !taken || owner == ws {
			e.owners[uid] = ws
			return uid, nil
		}
	}
	return 0, errors.New("sandbox: no free workspace users; delete idle workspaces")
}

func chownTree(root string, uid int) error {
	return filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		return os.Lchown(path, uid, uid)
	})
}

// dirSize sums regular file sizes under dir, stopping once the sum passes
// stopAt (the exact size no longer matters then).
func dirSize(dir string, stopAt int64) int64 {
	var total int64
	entries := 0
	errStop := errors.New("stop")
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if entries++; entries > 200000 {
			total = stopAt
			return errStop
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
			if total > stopAt {
				return errStop
			}
		}
		return nil
	})
	return total
}

// Run executes one command.
func (e *Executor) Run(ctx context.Context, req Request) (*Result, error) {
	if !validWorkspace(req.Workspace) {
		return nil, ErrInvalidWorkspace
	}
	if len(req.Command) > MaxCommandBytes {
		return nil, ErrCommandTooLong
	}
	if strings.TrimSpace(req.Command) == "" {
		return nil, ErrEmptyCommand
	}
	timeout := e.cfg.DefaultTimeout
	if req.TimeoutSeconds > 0 && time.Duration(req.TimeoutSeconds)*time.Second < e.cfg.MaxTimeout {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	} else if req.TimeoutSeconds > 0 {
		timeout = e.cfg.MaxTimeout
	}
	maxOut := e.cfg.MaxOutputBytes
	if req.MaxOutputBytes > 0 && req.MaxOutputBytes < maxOut {
		maxOut = req.MaxOutputBytes
	}

	select {
	case e.slots <- struct{}{}:
		defer func() { <-e.slots }()
	default:
		return nil, ErrBusy
	}
	if !e.lock(req.Workspace) {
		return nil, ErrBusy
	}
	defer e.unlock(req.Workspace)

	dir, uid, err := e.prepare(req.Workspace)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	_ = os.Chtimes(dir, now, now)

	fsize := e.cfg.MaxFileBytes
	overQuota := dirSize(dir, e.cfg.WorkspaceQuota) >= e.cfg.WorkspaceQuota
	if overQuota {
		fsize = 0 // writes fail, deleting still works
	}

	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// The helper (this binary) applies the limits and then execs the shell.
	cmd := exec.CommandContext(runCtx, e.cfg.HelperPath, HelperArg,
		strconv.Itoa(e.cfg.CPUSeconds), strconv.Itoa(e.cfg.MaxOpenFiles),
		strconv.FormatInt(fsize, 10), strconv.Itoa(e.cfg.MaxProcesses), e.cfg.Shell, req.Command)
	cmd.Dir = dir
	cmd.Env = e.commandEnv(dir)
	buf := newHeadTailBuffer(maxOut)
	cmd.Stdout, cmd.Stderr = buf, buf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if uid >= 0 {
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(uid)}
	}
	cmd.Cancel = func() error { killGroup(cmd.Process.Pid); return nil }
	cmd.WaitDelay = 3 * time.Second

	start := time.Now()
	runErr := cmd.Run()
	if cmd.Process != nil {
		killGroup(cmd.Process.Pid) // no daemons: reap anything left behind
	}
	_ = os.Chtimes(dir, time.Now(), time.Now())

	res := &Result{DurationMS: time.Since(start).Milliseconds(), WritesDisabled: overQuota}
	res.Output, res.OmittedBytes = buf.Render()
	if ctx.Err() != nil {
		return nil, ctx.Err() // caller went away
	}
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee):
		res.ExitCode = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal().String()
			res.ExitCode = 128 + int(ws.Signal())
		}
	case errors.Is(runErr, exec.ErrWaitDelay):
		// the command finished but a background process held the pipes open
	default:
		if errors.Is(runErr, os.ErrPermission) && uid >= 0 {
			return nil, fmt.Errorf("sandbox: starting command as user %d: %w (that user must be able to enter every directory above %s, e.g. mode 0711, and to execute %s)",
				uid, runErr, e.cfg.WorkDir, e.cfg.HelperPath)
		}
		return nil, fmt.Errorf("sandbox: starting command: %w", runErr)
	}
	return res, nil
}

func (e *Executor) commandEnv(dir string) []string {
	env := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"HOME=" + dir,
		"PWD=" + dir,
		"TMPDIR=" + filepath.Join(dir, ".tmp"),
		"LANG=C.UTF-8",
		"TERM=dumb",
	}
	return append(env, e.cfg.ExtraEnv...)
}

func killGroup(pid int) {
	if pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// Reset deletes a workspace and frees its user.
func (e *Executor) Reset(_ context.Context, ws string) error {
	if !validWorkspace(ws) {
		return ErrInvalidWorkspace
	}
	if !e.lock(ws) {
		return ErrBusy
	}
	defer e.unlock(ws)
	return e.remove(ws)
}

func (e *Executor) remove(ws string) error {
	dir := filepath.Join(e.cfg.WorkDir, ws)
	owner, owned := ownerOf(dir)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if e.isolate && owned && e.inRange(owner) {
		e.cleanTmp(owner)
	}
	e.mu.Lock()
	for uid, o := range e.owners {
		if o == ws {
			delete(e.owners, uid)
		}
	}
	e.mu.Unlock()
	return nil
}

// cleanTmp deletes the top-level entries of the shared temporary directory
// that belong to uid, so that a recycled user id cannot read what its
// previous owner left there.
func (e *Executor) cleanTmp(uid uint32) {
	entries, err := os.ReadDir(e.cfg.TmpDir)
	if err != nil {
		return
	}
	for _, ent := range entries {
		p := filepath.Join(e.cfg.TmpDir, ent.Name())
		if owner, ok := ownerOf(p); ok && owner == uid {
			_ = os.RemoveAll(p)
		}
	}
}

// Janitor deletes workspaces unused for IdleTTL until ctx is cancelled.
func (e *Executor) Janitor(ctx context.Context) {
	t := time.NewTicker(e.cfg.JanitorEvery)
	defer t.Stop()
	for {
		e.Sweep()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Sweep removes idle workspaces once and returns how many were deleted.
func (e *Executor) Sweep() int {
	entries, err := os.ReadDir(e.cfg.WorkDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, ent := range entries {
		if !ent.IsDir() || !validWorkspace(ent.Name()) {
			continue
		}
		fi, err := ent.Info()
		if err != nil || time.Since(fi.ModTime()) < e.cfg.IdleTTL {
			continue
		}
		if !e.lock(ent.Name()) {
			continue
		}
		if err := e.remove(ent.Name()); err != nil {
			e.log.Warn("removing idle workspace failed", "workspace", ent.Name(), "err", err)
		} else {
			removed++
		}
		e.unlock(ent.Name())
	}
	if removed > 0 {
		e.log.Info("removed idle workspaces", "count", removed)
	}
	return removed
}
