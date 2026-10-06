package main

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func makeData(t *testing.T) string {
	d := t.TempDir()
	p := filepath.Join(d, "sessions", "telegram", "a-1")
	if err := os.MkdirAll(p, 0o750); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(p, "session.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE t(v TEXT); INSERT INTO t VALUES ('hello')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	_ = os.WriteFile(filepath.Join(d, "web_secret"), []byte("0123456789abcdef0123456789abcdef"), 0o600)
	return d
}

func TestBackupVerifyRestoreCommands(t *testing.T) {
	data, out := makeData(t), t.TempDir()
	var so, se bytes.Buffer
	if code := runBackup([]string{"--data-dir", data, "--out", out, "--keep", "3"}, &so, &se); code != 0 || !strings.Contains(so.String(), "backup written") {
		t.Fatalf("%d %s %s", code, so.String(), se.String())
	}
	list, _ := filepath.Glob(filepath.Join(out, "jannyq-backup-*.tar.gz"))
	if len(list) != 1 {
		t.Fatalf("backups: %v", list)
	}
	so.Reset()
	if code := runVerify([]string{list[0]}, &so, &se); code != 0 || !strings.Contains(so.String(), "ok: 2 file(s)") {
		t.Fatalf("%d %s %s", code, so.String(), se.String())
	}
	dest := t.TempDir()
	so.Reset()
	if code := runRestore([]string{"--from", list[0], "--data-dir", dest}, &so, &se); code != 0 {
		t.Fatalf("%d %s", code, se.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "sessions", "telegram", "a-1", "session.db")); err != nil {
		t.Error(err)
	}
	// a second restore needs --force
	se.Reset()
	if code := runRestore([]string{"--from", list[0], "--data-dir", dest}, &so, &se); code != 1 || !strings.Contains(se.String(), "--force") {
		t.Errorf("%d %s", code, se.String())
	}
	if code := runRestore([]string{"--from", list[0], "--data-dir", dest, "--force"}, &so, &se); code != 0 {
		t.Errorf("force: %d", code)
	}
	// usage errors
	se.Reset()
	if code := runRestore(nil, &so, &se); code != 2 {
		t.Errorf("no --from: %d", code)
	}
	if code := runVerify(nil, &so, &se); code != 2 {
		t.Errorf("no file: %d", code)
	}
	if code := runBackup([]string{"--data-dir", filepath.Join(data, "missing"), "--out", out}, &so, &se); code != 1 {
		t.Errorf("a missing data directory: %d", code)
	}
	bad := filepath.Join(t.TempDir(), "x.tar.gz")
	_ = os.WriteFile(bad, []byte("nope"), 0o600)
	if code := runVerify([]string{bad}, &so, &se); code != 1 {
		t.Errorf("junk: %d", code)
	}
}
