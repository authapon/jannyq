package skill

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(s []Summary) string {
	var n []string
	for _, x := range s {
		n = append(n, x.Name)
	}
	return strings.Join(n, ",")
}

func TestParse(t *testing.T) {
	fm, body, err := parse([]byte("---\nname: demo\ndescription: >\n  Does a thing\n  over two lines.\n---\n\n# Steps\n1. go\n"))
	if err != nil || fm.Name != "demo" || fm.Description != "Does a thing over two lines." || body != "# Steps\n1. go" {
		t.Errorf("fm=%+v body=%q err=%v", fm, body, err)
	}
	if _, _, err := parse([]byte("# no front matter")); err == nil {
		t.Error("missing front matter must fail")
	}
	if _, _, err := parse([]byte("---\nname: x\n")); err == nil {
		t.Error("unclosed front matter must fail")
	}
	if _, _, err := parse([]byte("---\nname: [unclosed\n---\nbody")); err == nil {
		t.Error("invalid yaml must fail")
	}
	fm, body, err = parse([]byte("\xef\xbb\xbf---\r\ndescription: crlf\r\n---\r\nbody\r\n"))
	if err != nil || fm.Description != "crlf" || body != "body" {
		t.Errorf("BOM/CRLF: fm=%+v body=%q err=%v", fm, body, err)
	}
}

func TestStoreListsValidSkillsOnly(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "zeta/SKILL.md", "---\ndescription: last one\n---\nbody")
	write(t, dir, "alpha/SKILL.md", "---\nname: alpha\ndescription: first one\n---\nbody")
	write(t, dir, "nodesc/SKILL.md", "---\nname: nodesc\n---\nbody")
	write(t, dir, "broken/SKILL.md", "no front matter")
	write(t, dir, "Bad Name/SKILL.md", "---\ndescription: x\n---\nbody")
	write(t, dir, ".hidden/SKILL.md", "---\ndescription: x\n---\nbody")
	write(t, dir, "notaskill/readme.txt", "hello")
	write(t, dir, "stray-file.md", "---\ndescription: x\n---\n")

	s, err := NewStore(dir, "/skills", quiet())
	if err != nil {
		t.Fatal(err)
	}
	got := s.Summaries()
	if names(got) != "alpha,zeta" || got[0].Description != "first one" {
		t.Errorf("summaries = %+v", got)
	}
}

func TestStoreNeedsADirectory(t *testing.T) {
	if _, err := NewStore(filepath.Join(t.TempDir(), "missing"), "", nil); err == nil {
		t.Error("missing dir must fail")
	}
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o644)
	if _, err := NewStore(f, "", nil); err == nil {
		t.Error("a file is not a skills dir")
	}
}

func TestLoadBodyAndFileList(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "csv/SKILL.md", "---\ndescription: summarise csv files\n---\nRun the script.")
	write(t, dir, "csv/summarize.py", "print('hi')")
	write(t, dir, "csv/ref/format.md", "# format")
	write(t, dir, "csv/.secret", "hidden")
	s, _ := NewStore(dir, "/skills/", quiet())
	out, err := s.Load("csv", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Run the script.", "/skills/csv", "- summarize.py", "- ref/format.md"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".secret") || strings.Contains(out, "description:") {
		t.Errorf("output leaks hidden files or front matter:\n%s", out)
	}
	if got, err := s.Load("csv", "ref/format.md"); err != nil || got != "# format" {
		t.Errorf("file load: %q %v", got, err)
	}
}

func TestLoadRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "skills")
	write(t, dir, "s/SKILL.md", "---\ndescription: d\n---\nbody")
	write(t, root, "outside.txt", "TOP SECRET")
	write(t, dir, "other/SKILL.md", "---\ndescription: d\n---\nother")
	if err := os.Symlink(filepath.Join(root, "outside.txt"), filepath.Join(dir, "s", "link.txt")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_ = os.Symlink(root, filepath.Join(dir, "s", "uplink"))
	s, _ := NewStore(dir, "", quiet())
	for _, f := range []string{"../../outside.txt", "../other/SKILL.md", "/etc/passwd", "link.txt", "uplink/outside.txt", "..", ".", "..\\..\\outside.txt"} {
		out, err := s.Load("s", f)
		if err == nil || strings.Contains(out, "TOP SECRET") {
			t.Errorf("file %q: out=%q err=%v", f, out, err)
		}
	}
	if _, err := s.Load("../s", ""); err == nil {
		t.Error("path in skill name must fail")
	}
	if _, err := s.Load("missing", ""); err == nil || !strings.Contains(err.Error(), "no skill named") {
		t.Errorf("missing skill: %v", err)
	}
}

func TestLoadFileLimits(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "s/SKILL.md", "---\ndescription: d\n---\nbody")
	write(t, dir, "s/big.txt", strings.Repeat("a", maxFileSize+500))
	write(t, dir, "s/bin.dat", "ab\x00cd")
	write(t, dir, "s/bad.txt", "\xff\xfe\xfd")
	s, _ := NewStore(dir, "", quiet())
	out, err := s.Load("s", "big.txt")
	if err != nil || !strings.Contains(out, "[truncated at 64 KB]") || len(out) > maxFileSize+100 {
		t.Errorf("big file: len=%d err=%v", len(out), err)
	}
	for _, f := range []string{"bin.dat", "bad.txt"} {
		if _, err := s.Load("s", f); err == nil || !strings.Contains(err.Error(), "binary") {
			t.Errorf("%s: err = %v", f, err)
		}
	}
	if _, err := s.Load("s", "nope.txt"); err == nil {
		t.Error("missing file must fail")
	}
	if _, err := s.Load("s", "."); err == nil {
		t.Error("directory must fail")
	}
}

func TestStoreNoticesChanges(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "one/SKILL.md", "---\ndescription: first\n---\nbody")
	s, _ := NewStore(dir, "", quiet())
	if names(s.Summaries()) != "one" {
		t.Fatal("initial scan")
	}
	// a new skill is found immediately when it is asked for by name...
	write(t, dir, "two/SKILL.md", "---\ndescription: second\n---\nbody two")
	if out, err := s.Load("two", ""); err != nil || !strings.Contains(out, "body two") {
		t.Errorf("hot-added skill: %q %v", out, err)
	}
	// ...and listed once the scan interval has passed
	s.mu.Lock()
	s.scannedAt = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	write(t, dir, "one/SKILL.md", "---\ndescription: changed\n---\nbody")
	got := s.Summaries()
	if names(got) != "one,two" || got[0].Description != "changed" {
		t.Errorf("after rescan: %+v", got)
	}
	_ = os.RemoveAll(filepath.Join(dir, "two"))
	s.mu.Lock()
	s.scannedAt = time.Now().Add(-time.Hour)
	s.mu.Unlock()
	if names(s.Summaries()) != "one" {
		t.Error("removed skill still listed")
	}
}
