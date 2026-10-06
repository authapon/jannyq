//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var bg = context.Background()

// The executor starts commands by re-running its own binary (HelperArg). Under
// `go test` that binary is the test binary, so it must behave as the helper.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == HelperArg {
		os.Exit(HelperMain(os.Args[2:]))
	}
	os.Exit(m.Run())
}

var (
	helperOnce sync.Once
	helperPath string
)

// testHelper copies the test binary somewhere every user can execute it; the
// per-workspace users of the isolation tests cannot enter Go's private build dir.
func testHelper(t *testing.T) string {
	t.Helper()
	helperOnce.Do(func() {
		self, err := os.Executable()
		if err != nil {
			return
		}
		dir, err := os.MkdirTemp("", "sandbox-helper")
		if err != nil {
			return
		}
		_ = os.Chmod(dir, 0o755)
		data, err := os.ReadFile(self)
		if err != nil {
			return
		}
		p := filepath.Join(dir, "helper")
		if os.WriteFile(p, data, 0o755) == nil {
			helperPath = p
		}
	})
	if helperPath == "" {
		t.Fatal("cannot prepare the helper binary")
	}
	return helperPath
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// newExec builds an Executor in a temp dir. Tests that are not about user
// isolation run commands as the current user (AllowRoot).
func newExec(t *testing.T, tweak func(*ExecConfig)) *Executor {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	// per-workspace users must be able to reach the work dir
	for p := dir; strings.HasPrefix(p, os.TempDir()) && p != os.TempDir(); p = filepath.Dir(p) {
		_ = os.MkdirAll(p, 0o711)
		_ = os.Chmod(p, 0o711)
	}
	cfg := ExecConfig{WorkDir: dir, AllowRoot: true, MaxTimeout: 20 * time.Second, DefaultTimeout: 10 * time.Second, HelperPath: testHelper(t)}
	if tweak != nil {
		tweak(&cfg)
	}
	e, err := NewExecutor(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func run(t *testing.T, e *Executor, ws, cmd string) *Result {
	t.Helper()
	res, err := e.Run(bg, Request{Workspace: WorkspaceID(ws), Command: cmd})
	if err != nil {
		t.Fatalf("run %q: %v", cmd, err)
	}
	return res
}

func TestRunBasic(t *testing.T) {
	e := newExec(t, nil)
	res := run(t, e, "a", "echo out; echo err >&2; pwd; exit 3")
	if res.ExitCode != 3 || res.TimedOut {
		t.Errorf("res = %+v", res)
	}
	want := filepath.Join(e.cfg.WorkDir, WorkspaceID("a"))
	for _, s := range []string{"out", "err", want} {
		if !strings.Contains(res.Output, s) {
			t.Errorf("output lacks %q: %q", s, res.Output)
		}
	}
}

func TestWorkspacePersistsAndIsPerChat(t *testing.T) {
	e := newExec(t, nil)
	run(t, e, "a", "echo data > note.txt")
	if res := run(t, e, "a", "cat note.txt"); strings.TrimSpace(res.Output) != "data" {
		t.Errorf("file not kept: %q", res.Output)
	}
	if res := run(t, e, "b", "ls"); strings.Contains(res.Output, "note.txt") {
		t.Errorf("workspace b sees workspace a's file: %q", res.Output)
	}
}

func TestEnvironmentIsScrubbed(t *testing.T) {
	t.Setenv("JANNYQ_SANDBOX_TOKEN", "super-secret-token-value")
	t.Setenv("OTHER_SECRET", "leak-me")
	e := newExec(t, func(c *ExecConfig) { c.ExtraEnv = []string{"EXTRA=1"} })
	res := run(t, e, "a", "env")
	for _, bad := range []string{"super-secret", "leak-me", "JANNYQ"} {
		if strings.Contains(res.Output, bad) {
			t.Errorf("environment leaks %q:\n%s", bad, res.Output)
		}
	}
	for _, want := range []string{"PATH=", "HOME=" + filepath.Join(e.cfg.WorkDir, WorkspaceID("a")), "EXTRA=1", "TMPDIR="} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("environment lacks %q:\n%s", want, res.Output)
		}
	}
}

func TestStdinIsClosed(t *testing.T) {
	e := newExec(t, nil)
	start := time.Now()
	res := run(t, e, "a", "cat; echo done")
	if !strings.Contains(res.Output, "done") || time.Since(start) > 5*time.Second {
		t.Errorf("command waiting on stdin: %+v", res)
	}
}

// alive reports whether pid is a running process. Zombies (killed, but not
// yet reaped by an init that may not exist in the test environment) count
// as dead.
func alive(pid int) bool {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) {
			return s[i+2] != 'Z' && s[i+2] != 'X'
		}
	}
	return syscall.Kill(pid, 0) == nil
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("process %d is still alive", pid)
}

func readPID(t *testing.T, e *Executor, ws string) int {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.cfg.WorkDir, WorkspaceID(ws), "pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	e := newExec(t, nil)
	start := time.Now()
	res, err := e.Run(bg, Request{
		Workspace: WorkspaceID("a"), TimeoutSeconds: 1,
		Command: "sleep 60 & echo $! > pid; echo started; sleep 60",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.Signal != "killed" || res.ExitCode != 128+9 {
		t.Errorf("res = %+v", res)
	}
	if !strings.Contains(res.Output, "started") {
		t.Errorf("output before the timeout is lost: %q", res.Output)
	}
	if d := time.Since(start); d > 8*time.Second {
		t.Errorf("took %v", d)
	}
	waitDead(t, readPID(t, e, "a"))
}

func TestBackgroundProcessesAreReaped(t *testing.T) {
	e := newExec(t, nil)
	start := time.Now()
	res := run(t, e, "a", "sleep 60 & echo $! > pid; echo finished")
	if res.TimedOut || res.ExitCode != 0 || !strings.Contains(res.Output, "finished") {
		t.Errorf("res = %+v", res)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v", d)
	}
	waitDead(t, readPID(t, e, "a"))
}

func TestOutputIsBounded(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.MaxOutputBytes = 1000 })
	res := run(t, e, "a", "yes 'a line of output' | head -c 500000")
	if len(res.Output) > 1100 || res.OmittedBytes < 400000 || !strings.Contains(res.Output, "bytes omitted") {
		t.Errorf("len=%d omitted=%d", len(res.Output), res.OmittedBytes)
	}
	// a request may lower the cap but never raise it
	r2, err := e.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "yes | head -c 100000", MaxOutputBytes: 100})
	if err != nil || len(r2.Output) > 200 {
		t.Errorf("lowered cap: len=%d err=%v", len(r2.Output), err)
	}
	r3, _ := e.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "yes | head -c 100000", MaxOutputBytes: 10_000_000})
	if len(r3.Output) > 1100 {
		t.Errorf("raised cap: len=%d", len(r3.Output))
	}
}

func TestInvalidUTF8Output(t *testing.T) {
	e := newExec(t, nil)
	res := run(t, e, "a", `printf '\377\376ok\000end'`)
	if strings.ContainsRune(res.Output, 0) || !strings.HasSuffix(res.Output, "okend") {
		t.Errorf("output = %q", res.Output)
	}
}

func TestResourceLimitsApplied(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.CPUSeconds = 7; c.MaxOpenFiles = 33; c.MaxProcesses = 77; c.MaxFileBytes = 4096 })
	res := run(t, e, "a", "ulimit -t; ulimit -n; ulimit -c; ulimit -f")
	// -f counts 512-byte blocks in dash
	if got := strings.Fields(res.Output); len(got) != 4 || got[0] != "7" || got[1] != "33" || got[2] != "0" || got[3] != "8" {
		t.Errorf("ulimits = %q", res.Output)
	}
	if _, err := os.Stat("/proc/self/limits"); err == nil {
		res := run(t, e, "a", "grep -E 'Max processes|Max cpu time' /proc/self/limits")
		if !strings.Contains(res.Output, "77") {
			t.Errorf("process limit not applied:\n%s", res.Output)
		}
	}
}

func TestSingleFileSizeLimit(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.MaxFileBytes = 2048 })
	res := run(t, e, "a", "head -c 100000 /dev/zero > f; wc -c < f")
	if n, _ := strconv.Atoi(strings.TrimSpace(res.Output)); n > 2048 {
		t.Errorf("file grew to %d bytes: %q", n, res.Output)
	}
}

func TestQuotaDisablesWritesButAllowsCleanup(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.WorkspaceQuota = 10 << 10 })
	if res := run(t, e, "a", "head -c 20000 /dev/zero > big"); res.WritesDisabled {
		t.Fatalf("first run must be allowed: %+v", res)
	}
	res := run(t, e, "a", "echo hi > f; echo rc=$?; ls")
	if !res.WritesDisabled || strings.Contains(res.Output, "rc=0") {
		t.Errorf("over-quota writes must fail: %+v", res)
	}
	res = run(t, e, "a", "rm big && echo removed")
	if !strings.Contains(res.Output, "removed") {
		t.Errorf("cleanup must work when over quota: %+v", res)
	}
	if res := run(t, e, "a", "echo hi > f && echo ok"); res.WritesDisabled || !strings.Contains(res.Output, "ok") {
		t.Errorf("writes must work again: %+v", res)
	}
}

func TestBusyLimits(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.MaxConcurrent = 1 })
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		go func() { time.Sleep(200 * time.Millisecond); close(started) }()
		_, _ = e.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "sleep 1"})
	}()
	<-started
	if _, err := e.Run(bg, Request{Workspace: WorkspaceID("b"), Command: "true"}); !errors.Is(err, ErrBusy) {
		t.Errorf("global limit: err = %v", err)
	}
	<-done

	e2 := newExec(t, nil)
	go func() { _, _ = e2.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "sleep 1"}) }()
	time.Sleep(200 * time.Millisecond)
	if _, err := e2.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "true"}); !errors.Is(err, ErrBusy) {
		t.Errorf("per-workspace limit: err = %v", err)
	}
	if _, err := e2.Run(bg, Request{Workspace: WorkspaceID("b"), Command: "true"}); err != nil {
		t.Errorf("other workspaces must not be blocked: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
}

func TestRequestValidation(t *testing.T) {
	e := newExec(t, nil)
	for name, tc := range map[string]struct {
		req  Request
		want error
	}{
		"bad workspace":  {Request{Workspace: "../etc", Command: "true"}, ErrInvalidWorkspace},
		"short":          {Request{Workspace: "abc", Command: "true"}, ErrInvalidWorkspace},
		"uppercase":      {Request{Workspace: strings.ToUpper(WorkspaceID("x")), Command: "true"}, ErrInvalidWorkspace},
		"empty":          {Request{Workspace: WorkspaceID("x"), Command: "  \n"}, ErrEmptyCommand},
		"too long":       {Request{Workspace: WorkspaceID("x"), Command: strings.Repeat("a", MaxCommandBytes+1)}, ErrCommandTooLong},
		"reset bad name": {Request{}, ErrInvalidWorkspace},
	} {
		var err error
		if name == "reset bad name" {
			err = e.Reset(bg, "../../x")
		} else {
			_, err = e.Run(bg, tc.req)
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestTimeoutIsClamped(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.MaxTimeout = 2 * time.Second; c.DefaultTimeout = 2 * time.Second })
	start := time.Now()
	res, err := e.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "sleep 30", TimeoutSeconds: 3600})
	if err != nil || !res.TimedOut || time.Since(start) > 8*time.Second {
		t.Errorf("res=%+v err=%v after %v", res, err, time.Since(start))
	}
}

func TestCallerCancellation(t *testing.T) {
	e := newExec(t, nil)
	ctx, cancel := context.WithTimeout(bg, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := e.Run(ctx, Request{Workspace: WorkspaceID("a"), Command: "sleep 30"})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 6*time.Second {
		t.Errorf("err = %v after %v", err, time.Since(start))
	}
	// the workspace must be usable again afterwards
	if res := run(t, e, "a", "echo again"); !strings.Contains(res.Output, "again") {
		t.Errorf("res = %+v", res)
	}
}

func TestResetDeletesWorkspace(t *testing.T) {
	e := newExec(t, nil)
	run(t, e, "a", "echo x > f")
	if err := e.Reset(bg, WorkspaceID("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.WorkDir, WorkspaceID("a"))); !os.IsNotExist(err) {
		t.Errorf("workspace still exists: %v", err)
	}
	if res := run(t, e, "a", "ls"); strings.Contains(res.Output, "f") {
		t.Errorf("fresh workspace expected: %q", res.Output)
	}
	if err := e.Reset(bg, WorkspaceID("never-existed")); err != nil {
		t.Errorf("reset of a missing workspace must succeed: %v", err)
	}
}

func TestSweepRemovesOnlyIdleWorkspaces(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.IdleTTL = time.Hour })
	run(t, e, "old", "true")
	run(t, e, "new", "true")
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(e.cfg.WorkDir, WorkspaceID("old")), past, past); err != nil {
		t.Fatal(err)
	}
	if n := e.Sweep(); n != 1 {
		t.Errorf("removed %d, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.WorkDir, WorkspaceID("new"))); err != nil {
		t.Errorf("active workspace removed: %v", err)
	}
	// non-workspace entries are never touched
	other := filepath.Join(e.cfg.WorkDir, "keepme")
	_ = os.Mkdir(other, 0o755)
	_ = os.Chtimes(other, past, past)
	e.Sweep()
	if _, err := os.Stat(other); err != nil {
		t.Errorf("foreign directory removed: %v", err)
	}
}

func TestJanitorStopsWithContext(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.JanitorEvery = 10 * time.Millisecond })
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { e.Janitor(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("janitor did not stop")
	}
}

func TestInfo(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.Network = true; c.ToolCandidates = []string{"sh", "definitely-not-installed"} })
	info := e.Info()
	if !info.Network || info.MaxTimeout != 20 || info.DefaultTimeout != 10 || len(info.Tools) != 1 || info.Tools[0] != "sh" {
		t.Errorf("info = %+v", info)
	}
}

func TestRefusesToRunAsRootWithoutIsolation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("only relevant when running as root")
	}
	_, err := NewExecutor(ExecConfig{WorkDir: t.TempDir()}, quietLog())
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Errorf("err = %v", err)
	}
}

// --- per-workspace users (root only) ---

func newIsolated(t *testing.T) *Executor {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("per-workspace users need root")
	}
	return newExec(t, func(c *ExecConfig) { c.AllowRoot = false; c.UIDBase = 41000; c.UIDCount = 200 })
}

func uidOf(t *testing.T, e *Executor, ws string) int {
	t.Helper()
	res := run(t, e, ws, "id -u")
	n, err := strconv.Atoi(strings.TrimSpace(res.Output))
	if err != nil {
		t.Fatalf("id -u: %q", res.Output)
	}
	return n
}

func TestEachWorkspaceGetsItsOwnUnprivilegedUser(t *testing.T) {
	e := newIsolated(t)
	a, b := uidOf(t, e, "a"), uidOf(t, e, "b")
	if a == 0 || b == 0 || a == b || a < 41000 || b < 41000 || a >= 41200 || b >= 41200 {
		t.Fatalf("uids a=%d b=%d", a, b)
	}
	if again := uidOf(t, e, "a"); again != a {
		t.Errorf("uid changed from %d to %d", a, again)
	}
	// survives an executor restart
	e2, err := NewExecutor(e.cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if again := uidOf(t, e2, "a"); again != a {
		t.Errorf("uid after restart %d, want %d", again, a)
	}
	if other := uidOf(t, e2, "c"); other == a || other == b {
		t.Errorf("new workspace reused uid %d", other)
	}
}

func TestWorkspacesCannotReadEachOther(t *testing.T) {
	e := newIsolated(t)
	run(t, e, "a", "echo secret-of-a > private.txt; chmod 644 private.txt")
	wsA := WorkspaceID("a")
	res := run(t, e, "b", fmt.Sprintf("cat ../%s/private.txt; echo rc=$?; ls ../%s; echo rc=$?; echo x > ../%s/evil; echo rc=$?", wsA, wsA, wsA))
	if strings.Contains(res.Output, "secret-of-a") || strings.Contains(res.Output, "rc=0") {
		t.Errorf("workspace b reached workspace a:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "Permission denied") {
		t.Errorf("expected permission errors:\n%s", res.Output)
	}
	// a can still use its own files
	if r := run(t, e, "a", "cat private.txt"); !strings.Contains(r.Output, "secret-of-a") {
		t.Errorf("a lost its file: %q", r.Output)
	}
}

func TestWorkspacesCannotSignalEachOther(t *testing.T) {
	e := newIsolated(t)
	pidFile := filepath.Join(os.TempDir(), fmt.Sprintf("jannyq-pid-%d", time.Now().UnixNano()))
	defer os.Remove(pidFile)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = e.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "echo $$ > " + pidFile + "; chmod 644 " + pidFile + "; sleep 3"})
	}()
	for i := 0; i < 100; i++ {
		if b, _ := os.ReadFile(pidFile); len(strings.TrimSpace(string(b))) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	res := run(t, e, "b", "kill -9 $(cat "+pidFile+"); echo rc=$?")
	if strings.Contains(res.Output, "rc=0") || !strings.Contains(res.Output, "not permitted") {
		t.Errorf("workspace b could signal workspace a:\n%s", res.Output)
	}
	<-done
}

func TestCannotReadExecutorEnvironment(t *testing.T) {
	e := newIsolated(t)
	t.Setenv("JANNYQ_SANDBOX_TOKEN", "must-not-leak")
	// the test process itself plays the executor: its environ must be unreadable
	res := run(t, e, "a", fmt.Sprintf("cat /proc/%d/environ | head -c 50; echo rc=$?", os.Getpid()))
	if strings.Contains(res.Output, "must-not-leak") || !strings.Contains(res.Output, "Permission denied") {
		t.Errorf("executor environment readable from a command:\n%s", res.Output)
	}
}

func TestExistingRootOwnedWorkspaceIsAdopted(t *testing.T) {
	e := newIsolated(t)
	dir := filepath.Join(e.cfg.WorkDir, WorkspaceID("legacy"))
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := run(t, e, "legacy", "cat sub/f.txt"); strings.TrimSpace(res.Output) != "old" {
		t.Errorf("adopted workspace unreadable: %q", res.Output)
	}
}

func TestUIDExhaustion(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	e := newExec(t, func(c *ExecConfig) { c.AllowRoot = false; c.UIDBase = 42000; c.UIDCount = 2 })
	run(t, e, "a", "true")
	run(t, e, "b", "true")
	if _, err := e.Run(bg, Request{Workspace: WorkspaceID("c"), Command: "true"}); err == nil || !strings.Contains(err.Error(), "no free workspace users") {
		t.Errorf("err = %v", err)
	}
	if err := e.Reset(bg, WorkspaceID("a")); err != nil {
		t.Fatal(err)
	}
	run(t, e, "c", "true") // the freed user is reusable
}

var _ sync.Locker = (*sync.Mutex)(nil)

func TestForkBombIsContainedPerWorkspace(t *testing.T) {
	e := newIsolated(t)
	e.cfg.MaxProcesses = 40

	// The bomb tries to start 300 background processes and reports its progress.
	// "exec sleep" replaces the shell instead of forking, so it still works at the limit.
	bomb := make(chan *Result, 1)
	go func() {
		res, _ := e.Run(bg, Request{Workspace: WorkspaceID("bomb"), TimeoutSeconds: 15, Command: `
( i=0; while [ $i -lt 300 ]; do echo $i; sleep 6 & i=$((i+1)); done ) 2>&1
exec sleep 3`})
		bomb <- res
	}()
	time.Sleep(1200 * time.Millisecond)

	// while the bomb's processes are alive, another workspace is unaffected
	other := run(t, e, "calm", "sleep 0.2; echo fine")
	if !strings.Contains(other.Output, "fine") {
		t.Errorf("a fork bomb in one workspace starved another: %+v", other)
	}

	res := <-bomb
	if res == nil {
		t.Fatal("bomb run failed")
	}
	if !strings.Contains(res.Output, "Cannot fork") && !strings.Contains(res.Output, "retry") && !strings.Contains(res.Output, "Resource temporarily unavailable") {
		t.Errorf("the process limit was never hit:\n%s", res.Output)
	}
	last := -1
	for _, f := range strings.Fields(res.Output) {
		if n, err := strconv.Atoi(f); err == nil {
			last = n
		}
	}
	if last < 5 || last > 60 {
		t.Errorf("%d processes were started with a limit of 40:\n%s", last, res.Output)
	}
}

func TestRecycledUserCannotReadTmpLeftovers(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	tmp := t.TempDir()
	if err := os.Chmod(tmp, 0o1777); err != nil {
		t.Fatal(err)
	}
	e := newExec(t, func(c *ExecConfig) {
		c.AllowRoot = false
		c.UIDBase, c.UIDCount = 43000, 1 // a single user id, so it is necessarily recycled
		c.TmpDir = tmp
	})
	secret := filepath.Join(tmp, "left-by-a")
	run(t, e, "a", "echo old-secret > "+secret+"; mkdir "+tmp+"/dir-by-a; echo x > "+tmp+"/dir-by-a/f; chmod 644 "+secret)
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if err := e.Reset(bg, WorkspaceID("a")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{secret, tmp + "/dir-by-a"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s survived the reset of its owner: %v", p, err)
		}
	}

	// the same cleanup happens when a user id is handed to a new workspace
	// (e.g. the old one was removed by the janitor or a crash)
	run(t, e, "b", "echo b-secret > "+tmp+"/left-by-b; chmod 644 "+tmp+"/left-by-b")
	_ = os.RemoveAll(filepath.Join(e.cfg.WorkDir, WorkspaceID("b"))) // bypasses Reset
	e2, err := NewExecutor(e.cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	res := run(t, e2, "c", "cat "+tmp+"/left-by-b 2>&1; true")
	if strings.Contains(res.Output, "b-secret") {
		t.Errorf("new owner of a recycled user id read the old owner's /tmp file: %q", res.Output)
	}
}
