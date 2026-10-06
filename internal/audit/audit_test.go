package audit

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func readEntries(t *testing.T, path string) []Entry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func TestLogAppendsJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "cmds.jsonl")
	l, err := Open(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	_ = l.Log(Entry{Channel: "telegram", Chat: "telegram:1", UserID: "7", Command: "ls -la", ExitCode: 0})
	_ = l.Log(Entry{Channel: "telegram", UserID: "8", Command: "sleep 99", TimedOut: true, Signal: "killed", ExitCode: 137})
	es := readEntries(t, path)
	if len(es) != 2 || es[0].Command != "ls -la" || es[0].Time.IsZero() || !es[1].TimedOut || es[1].UserID != "8" {
		t.Errorf("entries = %+v", es)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
}

func TestLongCommandsAreTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	l, _ := Open(path, 0)
	defer l.Close()
	_ = l.Log(Entry{Command: strings.Repeat("ก", 10000)})
	if es := readEntries(t, path); len([]rune(es[0].Command)) > maxCommandRunes+1 {
		t.Errorf("command not truncated: %d runes", len([]rune(es[0].Command)))
	}
}

func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	l, _ := Open(path, 500)
	defer l.Close()
	for i := 0; i < 60; i++ {
		if err := l.Log(Entry{Command: "echo " + strings.Repeat("x", 40)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{path, path + ".1", path + ".2", path + ".3"} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("missing %s: %v", p, err)
		}
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Error("more than three rotated files kept")
	}
	if fi, _ := os.Stat(path); fi.Size() > 600 {
		t.Errorf("active file grew to %d", fi.Size())
	}
}

func TestConcurrentLogging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.jsonl")
	l, _ := Open(path, 0)
	defer l.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				_ = l.Log(Entry{Command: "echo"})
			}
		}()
	}
	wg.Wait()
	if n := len(readEntries(t, path)); n != 500 {
		t.Errorf("entries = %d, want 500 (lines must not interleave)", n)
	}
}

func TestNilAndClosedLogger(t *testing.T) {
	var nl *Logger
	if err := nl.Log(Entry{}); err != nil || nl.Close() != nil {
		t.Error("nil logger must be a no-op")
	}
	l, _ := Open(filepath.Join(t.TempDir(), "c.jsonl"), 0)
	l.Close()
	if err := l.Log(Entry{}); err == nil {
		t.Error("logging after Close must fail")
	}
}
