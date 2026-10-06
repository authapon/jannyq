//go:build unix

package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func put(t *testing.T, e *Executor, ws, path, content string) {
	t.Helper()
	if err := e.Put(bg, WorkspaceID(ws), path, strings.NewReader(content), 0); err != nil {
		t.Fatalf("put %q: %v", path, err)
	}
}

func get(t *testing.T, e *Executor, ws, path string) string {
	t.Helper()
	b, err := e.Get(bg, WorkspaceID(ws), path, 0)
	if err != nil {
		t.Fatalf("get %q: %v", path, err)
	}
	return string(b)
}

func TestPutGetRemoveRoundTrip(t *testing.T) {
	e := newExec(t, nil)
	put(t, e, "a", "inbox/report.pdf", "%PDF-1.4 hello")
	put(t, e, "a", "deep/er/still/file.txt", "nested")
	put(t, e, "a", "inbox/report.pdf", "replaced") // overwrite
	if got := get(t, e, "a", "inbox/report.pdf"); got != "replaced" {
		t.Errorf("got %q", got)
	}
	if got := get(t, e, "a", "deep/er/still/file.txt"); got != "nested" {
		t.Errorf("got %q", got)
	}
	// commands in the workspace see the files
	if res := run(t, e, "a", "cat inbox/report.pdf; ls deep/er/still"); !strings.Contains(res.Output, "replaced") || !strings.Contains(res.Output, "file.txt") {
		t.Errorf("a command does not see the uploaded files: %q", res.Output)
	}
	// ...and files they create can be downloaded
	run(t, e, "a", "echo made-by-command > out.txt")
	if got := get(t, e, "a", "out.txt"); strings.TrimSpace(got) != "made-by-command" {
		t.Errorf("got %q", got)
	}
	// other workspaces cannot see them
	if _, err := e.Get(bg, WorkspaceID("b"), "inbox/report.pdf", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("another workspace: %v", err)
	}
	if err := e.Remove(bg, WorkspaceID("a"), "inbox/report.pdf"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Get(bg, WorkspaceID("a"), "inbox/report.pdf", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("after remove: %v", err)
	}
	if err := e.Remove(bg, WorkspaceID("a"), "inbox/report.pdf"); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing twice: %v", err)
	}
}

func TestBinaryAndLargeFilesSurvive(t *testing.T) {
	e := newExec(t, nil)
	data := make([]byte, 5<<20+17)
	for i := range data {
		data[i] = byte(i * 31)
	}
	if err := e.Put(bg, WorkspaceID("a"), "big.bin", bytes.NewReader(data), 0); err != nil {
		t.Fatal(err)
	}
	got, err := e.Get(bg, WorkspaceID("a"), "big.bin", 0)
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("round trip failed: err=%v len=%d", err, len(got))
	}
}

func TestPathsCannotLeaveTheWorkspace(t *testing.T) {
	e := newExec(t, nil)
	outside := filepath.Join(filepath.Dir(e.cfg.WorkDir), "outside.txt")
	if err := os.WriteFile(outside, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	put(t, e, "victim", "private.txt", "victim's data")
	victim := filepath.Join(e.cfg.WorkDir, WorkspaceID("victim"), "private.txt")

	for _, p := range []string{
		"../outside.txt", "../../etc/passwd", "a/../../outside.txt", "/etc/passwd", "", ".", "..", "a/..",
		"x\x00y", strings.Repeat("a/", 300) + "f", `a\..\..\x`, "../" + WorkspaceID("victim") + "/private.txt",
	} {
		if err := e.Put(bg, WorkspaceID("a"), p, strings.NewReader("pwned"), 0); !errors.Is(err, ErrBadPath) {
			t.Errorf("Put(%q) = %v, want ErrBadPath", p, err)
		}
		if b, err := e.Get(bg, WorkspaceID("a"), p, 0); err == nil || strings.Contains(string(b), "SECRET") {
			t.Errorf("Get(%q) = %q, %v", p, b, err)
		}
		if err := e.Remove(bg, WorkspaceID("a"), p); err == nil {
			t.Errorf("Remove(%q) succeeded", p)
		}
	}
	if b, _ := os.ReadFile(outside); string(b) != "TOP SECRET" {
		t.Error("a file outside the workspaces was modified")
	}
	if b, _ := os.ReadFile(victim); string(b) != "victim's data" {
		t.Error("another workspace's file was modified")
	}
}

func TestSymlinksCannotBeUsedToEscape(t *testing.T) {
	e := newExec(t, nil)
	secret := filepath.Join(filepath.Dir(e.cfg.WorkDir), "secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a command (which the user controls) plants symlinks inside its workspace
	run(t, e, "a", fmt.Sprintf("ln -s %s link.txt; ln -s %s dirlink; ln -s ../%s up", secret, filepath.Dir(e.cfg.WorkDir), WorkspaceID("other")))
	put(t, e, "other", "data.txt", "other workspace")

	if b, err := e.Get(bg, WorkspaceID("a"), "link.txt", 0); err == nil || strings.Contains(string(b), "SECRET") {
		t.Errorf("read through a symlink to a file outside: %q %v", b, err)
	}
	if b, err := e.Get(bg, WorkspaceID("a"), "dirlink/secret.txt", 0); err == nil || strings.Contains(string(b), "SECRET") {
		t.Errorf("read through a symlinked directory: %q %v", b, err)
	}
	if b, err := e.Get(bg, WorkspaceID("a"), "up/data.txt", 0); err == nil || strings.Contains(string(b), "other workspace") {
		t.Errorf("read another workspace through a symlink: %q %v", b, err)
	}
	if err := e.Put(bg, WorkspaceID("a"), "link.txt", strings.NewReader("overwritten"), 0); err == nil {
		t.Error("wrote through a symlink to a file outside")
	}
	if err := e.Put(bg, WorkspaceID("a"), "dirlink/new.txt", strings.NewReader("planted"), 0); err == nil {
		t.Error("created a file through a symlinked directory")
	}
	if b, _ := os.ReadFile(secret); string(b) != "TOP SECRET" {
		t.Errorf("the file outside was changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(e.cfg.WorkDir), "new.txt")); err == nil {
		t.Error("a file was created outside the workspace")
	}
}

func TestFileSizeAndQuotaLimits(t *testing.T) {
	e := newExec(t, func(c *ExecConfig) { c.UploadMaxBytes = 1000; c.WorkspaceQuota = 2500 })
	if err := e.Put(bg, WorkspaceID("a"), "ok.bin", bytes.NewReader(make([]byte, 1000)), 0); err != nil {
		t.Fatalf("a file at the limit: %v", err)
	}
	if err := e.Put(bg, WorkspaceID("a"), "big.bin", bytes.NewReader(make([]byte, 1001)), 0); !errors.Is(err, ErrTooLarge) {
		t.Errorf("one byte over: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.WorkDir, WorkspaceID("a"), "big.bin")); err == nil {
		t.Error("a rejected upload left a partial file behind")
	}
	if err := e.Put(bg, WorkspaceID("a"), "caller-limit.bin", bytes.NewReader(make([]byte, 600)), 500); !errors.Is(err, ErrTooLarge) {
		t.Errorf("the caller's smaller limit: %v", err)
	}
	if _, err := e.Get(bg, WorkspaceID("a"), "ok.bin", 999); !errors.Is(err, ErrTooLarge) {
		t.Errorf("Get with a smaller limit: %v", err)
	}
	// the workspace quota: 2500 bytes in total
	_ = e.Put(bg, WorkspaceID("a"), "second.bin", bytes.NewReader(make([]byte, 1000)), 0)
	if err := e.Put(bg, WorkspaceID("a"), "third.bin", bytes.NewReader(make([]byte, 900)), 0); !errors.Is(err, ErrQuota) {
		t.Errorf("past the quota: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.WorkDir, WorkspaceID("a"), "third.bin")); err == nil {
		t.Error("an upload that exceeded the quota left a partial file")
	}
	if err := e.Remove(bg, WorkspaceID("a"), "ok.bin"); err != nil {
		t.Fatal(err)
	}
	if err := e.Put(bg, WorkspaceID("a"), "third.bin", bytes.NewReader(make([]byte, 900)), 0); err != nil {
		t.Errorf("after freeing space: %v", err)
	}
}

func TestGetRefusesDirectories(t *testing.T) {
	e := newExec(t, nil)
	put(t, e, "a", "dir/file", "x")
	if _, err := e.Get(bg, WorkspaceID("a"), "dir", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get of a directory: %v", err)
	}
	if err := e.Remove(bg, WorkspaceID("a"), "dir"); !errors.Is(err, ErrBadPath) {
		t.Errorf("Remove of a directory: %v", err)
	}
	if _, err := e.Get(bg, "../bad", "x", 0); !errors.Is(err, ErrInvalidWorkspace) {
		t.Errorf("bad workspace: %v", err)
	}
}

func TestFilesWaitForNoRunningCommand(t *testing.T) {
	e := newExec(t, nil)
	go func() { _, _ = e.Run(bg, Request{Workspace: WorkspaceID("a"), Command: "sleep 1"}) }()
	time.Sleep(200 * time.Millisecond)
	if err := e.Put(bg, WorkspaceID("a"), "f", strings.NewReader("x"), 0); !errors.Is(err, ErrBusy) {
		t.Errorf("Put while a command runs: %v", err)
	}
	if err := e.Put(bg, WorkspaceID("b"), "f", strings.NewReader("x"), 0); err != nil {
		t.Errorf("another workspace must not be blocked: %v", err)
	}
	time.Sleep(1200 * time.Millisecond)
}

func TestConcurrentUploadsToDifferentWorkspaces(t *testing.T) {
	e := newExec(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws := fmt.Sprintf("chat%d", i)
			content := strings.Repeat(fmt.Sprint(i), 10000)
			if err := e.Put(bg, WorkspaceID(ws), "inbox/f.txt", strings.NewReader(content), 0); err != nil {
				t.Errorf("%s: %v", ws, err)
				return
			}
			if b, err := e.Get(bg, WorkspaceID(ws), "inbox/f.txt", 0); err != nil || string(b) != content {
				t.Errorf("%s: wrong content (%v)", ws, err)
			}
		}()
	}
	wg.Wait()
}

func TestUploadedFilesBelongToTheWorkspaceUser(t *testing.T) {
	e := newIsolated(t)
	put(t, e, "a", "inbox/doc.txt", "mine")
	// the workspace's own user can read and write the files and directories...
	res := run(t, e, "a", "cat inbox/doc.txt; echo more >> inbox/doc.txt; touch inbox/new.txt; echo rc=$?")
	if !strings.Contains(res.Output, "mine") || !strings.Contains(res.Output, "rc=0") {
		t.Errorf("the workspace user cannot use its files: %q", res.Output)
	}
	// ...and no other workspace's user can
	wsA := WorkspaceID("a")
	res = run(t, e, "b", fmt.Sprintf("cat ../%s/inbox/doc.txt; echo rc=$?", wsA))
	if strings.Contains(res.Output, "mine") || !strings.Contains(res.Output, "Permission denied") {
		t.Errorf("another workspace could read the upload: %q", res.Output)
	}
	// the executor created them with owner-only permissions
	fi, err := os.Stat(filepath.Join(e.cfg.WorkDir, wsA, "inbox", "doc.txt"))
	if err != nil || fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("mode = %v err = %v", fi.Mode(), err)
	}
}

func TestFileAPIOverHTTP(t *testing.T) {
	e, c, ts := newPair(t, nil)
	ws := WorkspaceID("chat")
	info, err := c.Info(bg)
	if err != nil || !info.Files {
		t.Fatalf("info = %+v err = %v", info, err)
	}
	if err := c.Put(bg, ws, "inbox/a b&c.txt", strings.NewReader("hello over http"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := c.Get(bg, ws, "inbox/a b&c.txt", 0); err != nil || string(got) != "hello over http" {
		t.Errorf("get = %q, %v", got, err)
	}
	if b, _ := os.ReadFile(filepath.Join(e.cfg.WorkDir, ws, "inbox", "a b&c.txt")); string(b) != "hello over http" {
		t.Errorf("file on disk = %q", b)
	}
	if _, err := c.Get(bg, ws, "nope.txt", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	if err := c.Put(bg, ws, "../escape", strings.NewReader("x"), 0); !errors.Is(err, ErrBadPath) {
		t.Errorf("traversal: %v", err)
	}
	if err := c.Put(bg, ws, "limited", strings.NewReader("0123456789"), 5); !errors.Is(err, ErrTooLarge) {
		t.Errorf("client-side limit: %v", err)
	}
	if err := c.Remove(bg, ws, "inbox/a b&c.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get(bg, ws, "inbox/a b&c.txt", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("after remove: %v", err)
	}
	// the file endpoints are as protected as the rest
	bad := &Client{BaseURL: c.BaseURL, Token: "wrong-token-wrong-token", HTTP: ts.Client()}
	if err := bad.Put(bg, ws, "x", strings.NewReader("x"), 0); !errors.Is(err, ErrUnavailable) {
		t.Errorf("upload with a wrong token: %v", err)
	}
	if _, err := bad.Get(bg, ws, "x", 0); !errors.Is(err, ErrUnavailable) {
		t.Errorf("download with a wrong token: %v", err)
	}
	// an executor limit surfaces as ErrTooLarge over HTTP as well
	e.cfg.UploadMaxBytes = 100
	if err := c.Put(bg, ws, "big", bytes.NewReader(make([]byte, 101)), 0); !errors.Is(err, ErrTooLarge) {
		t.Errorf("server-side limit: %v", err)
	}
}
