package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var bg = context.Background()

func mkdb(t *testing.T, path string, rows ...string) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS t(v TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO t(v) VALUES (?)`, r); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func values(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT v FROM t ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		_ = rows.Scan(&v)
		out = append(out, v)
	}
	return strings.Join(out, ",")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// dataDir builds a small data directory with two chats, a knowledge base and secrets.
func dataDir(t *testing.T) (string, []*sql.DB) {
	d := t.TempDir()
	a := mkdb(t, filepath.Join(d, "sessions", "telegram", "alice-1", "session.db"), "alice one", "alice two")
	b := mkdb(t, filepath.Join(d, "sessions", "web", "bob-2", "session.db"), "bob one")
	mkdb(t, filepath.Join(d, "knowledge.db"), "passage")
	write(t, filepath.Join(d, "sessions", "telegram", "alice-1", "files", "1-report.pdf"), "%PDF-1.4 data")
	write(t, filepath.Join(d, "sessions", "telegram", "alice-1", "files", "2-photo.jpg"), "jpeg bytes")
	write(t, filepath.Join(d, "web_secret"), "0123456789abcdef0123456789abcdef")
	write(t, filepath.Join(d, "audit", "commands.jsonl"), `{"cmd":"ls"}`+"\n")
	return d, []*sql.DB{a, b}
}

func TestRoundTrip(t *testing.T) {
	d, _ := dataDir(t)
	out := t.TempDir()
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	p, err := Create(bg, out, Options{DataDir: d, Files: true, Version: "1.2.3", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(p) != "jannyq-backup-20261006T120000Z.tar.gz" {
		t.Errorf("name = %s", p)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Errorf("a backup holds private conversations; mode = %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o700 && fi.Mode().Perm() != 0o755 { // the directory existed already
		t.Logf("directory mode %v", fi.Mode().Perm())
	}
	if leftovers, _ := filepath.Glob(filepath.Join(out, ".*")); len(leftovers) != 0 {
		t.Errorf("scratch files left behind: %v", leftovers)
	}
	man, err := Verify(p, t.TempDir())
	if err != nil || man.JannyqVer != "1.2.3" || !man.IncludesFil || len(man.Files) != 7 {
		t.Fatalf("%v %+v", err, man)
	}

	dest := t.TempDir()
	if _, err := Restore(p, dest, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := values(t, filepath.Join(dest, "sessions", "telegram", "alice-1", "session.db")); got != "alice one,alice two" {
		t.Errorf("alice = %q", got)
	}
	if got := values(t, filepath.Join(dest, "sessions", "web", "bob-2", "session.db")); got != "bob one" {
		t.Errorf("bob = %q", got)
	}
	if got := values(t, filepath.Join(dest, "knowledge.db")); got != "passage" {
		t.Errorf("knowledge = %q", got)
	}
	if read(t, filepath.Join(dest, "sessions", "telegram", "alice-1", "files", "1-report.pdf")) != "%PDF-1.4 data" ||
		read(t, filepath.Join(dest, "web_secret")) != "0123456789abcdef0123456789abcdef" ||
		!strings.Contains(read(t, filepath.Join(dest, "audit", "commands.jsonl")), "ls") {
		t.Error("files do not match")
	}
	for _, rel := range []string{"web_secret", "knowledge.db", "sessions/web/bob-2/session.db"} {
		if fi, err := os.Stat(filepath.Join(dest, rel)); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v", rel, fi, err)
		}
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 4 {
		t.Errorf("the destination holds %d entries (staging left behind?)", len(entries))
	}
}

func TestFilesCanBeLeftOut(t *testing.T) {
	d, _ := dataDir(t)
	p, err := Create(bg, t.TempDir(), Options{DataDir: d, Files: false})
	if err != nil {
		t.Fatal(err)
	}
	man, err := Verify(p, t.TempDir())
	if err != nil || man.IncludesFil {
		t.Fatalf("%v %+v", err, man)
	}
	for _, f := range man.Files {
		if strings.Contains(f.Path, "/files/") {
			t.Errorf("%s was included", f.Path)
		}
	}
}

func TestSnapshotOfDatabasesBeingWritten(t *testing.T) {
	d, dbs := dataDir(t)
	// keep writing to the databases while the backup runs
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = dbs[0].Exec(`INSERT INTO t(v) VALUES (?)`, "live")
			time.Sleep(time.Millisecond)
		}
	}()
	p, err := Create(bg, t.TempDir(), Options{DataDir: d, Files: true})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(p, t.TempDir()); err != nil { // includes PRAGMA integrity_check of every database
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := Restore(p, dest, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	got := values(t, filepath.Join(dest, "sessions", "telegram", "alice-1", "session.db"))
	if !strings.HasPrefix(got, "alice one,alice two") {
		t.Errorf("the committed rows are missing: %q", got)
	}
}

func TestRestoreRefusesToOverwriteWithoutForce(t *testing.T) {
	d, _ := dataDir(t)
	p, _ := Create(bg, t.TempDir(), Options{DataDir: d, Files: true})
	dest := t.TempDir()
	mkdb(t, filepath.Join(dest, "sessions", "telegram", "carol-3", "session.db"), "carol")
	write(t, filepath.Join(dest, "web_secret"), "old secret")
	_, err := Restore(p, dest, RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("err = %v", err)
	}
	if read(t, filepath.Join(dest, "web_secret")) != "old secret" {
		t.Error("the refusal changed the destination")
	}
	if ents, _ := os.ReadDir(dest); len(ents) != 2 {
		t.Errorf("staging left behind: %v", ents)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := Restore(p, dest, RestoreOptions{Force: true, Now: func() time.Time { return now }}); err != nil {
		t.Fatal(err)
	}
	if values(t, filepath.Join(dest, "sessions", "web", "bob-2", "session.db")) != "bob one" {
		t.Error("not restored")
	}
	if read(t, filepath.Join(dest, "web_secret.before-restore-20260102T030405Z")) != "old secret" {
		t.Error("the old data was not moved aside")
	}
	if values(t, filepath.Join(dest, "sessions.before-restore-20260102T030405Z", "telegram", "carol-3", "session.db")) != "carol" {
		t.Error("the old chats were lost")
	}
}

// archive builds a tar.gz from entries; a zero-size name with typeflag is for special files.
type entry struct {
	name     string
	body     string
	typeflag byte
	link     string
}

func buildArchive(t *testing.T, entries []entry, manifest *Manifest) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	man := Manifest{Version: manifestVersion, Created: time.Now()}
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Mode: 0o600, Typeflag: tf, Linkname: e.link}
		if tf == tar.TypeReg {
			hdr.Size = int64(len(e.body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if tf == tar.TypeReg {
			_, _ = tw.Write([]byte(e.body))
			sum := sha256.Sum256([]byte(e.body))
			man.Files = append(man.Files, FileEntry{Path: e.name, Size: int64(len(e.body)), SHA256: hex.EncodeToString(sum[:])})
		}
	}
	if manifest != nil {
		man = *manifest
	}
	if manifest == nil || manifest.Version != -1 {
		mj, _ := json.Marshal(man)
		_ = tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mj)), Typeflag: tar.TypeReg})
		_, _ = tw.Write(mj)
	}
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "evil.tar.gz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHostileArchivesAreRefusedAndTouchNothing(t *testing.T) {
	parent := t.TempDir()
	dest := filepath.Join(parent, "data")
	outside := filepath.Join(parent, "outside.txt")
	cases := map[string][]entry{
		"parent directory":     {{name: "sessions/../../outside.txt", body: "x"}},
		"absolute path":        {{name: "/etc/passwd", body: "x"}},
		"dot dot at the start": {{name: "../outside.txt", body: "x"}},
		"backslash":            {{name: `sessions\..\outside.txt`, body: "x"}},
		"unknown top level":    {{name: "evil.sh", body: "x"}},
		"hidden in sessions":   {{name: "sessions/./x/../y/session.db", body: "x"}},
		"unclean path":         {{name: "sessions//a/session.db", body: "x"}},
		"symlink":              {{name: "sessions/a/link", typeflag: tar.TypeSymlink, link: "/etc/passwd"}},
		"hard link":            {{name: "sessions/a/hard", typeflag: tar.TypeLink, link: "knowledge.db"}},
		"device":               {{name: "sessions/a/dev", typeflag: tar.TypeChar}},
		"duplicate":            {{name: "web_secret", body: "a"}, {name: "web_secret", body: "b"}},
		"too deep":             {{name: "sessions/a/b/c/d/e/f/g/h", body: "x"}},
	}
	for name, entries := range cases {
		p := buildArchive(t, entries, nil)
		if _, err := Restore(p, dest, RestoreOptions{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if _, err := Verify(p, t.TempDir()); err == nil {
			t.Errorf("%s: verify accepted", name)
		}
		if _, err := os.Stat(outside); err == nil {
			t.Fatalf("%s: a file was written outside the destination", name)
		}
	}
	// nothing is left in the destination by any of the refused archives
	if ents, _ := os.ReadDir(dest); len(ents) != 0 {
		t.Errorf("leftovers: %v", ents)
	}
}

func TestDamagedAndIncompleteArchives(t *testing.T) {
	good := []entry{{name: "web_secret", body: "0123456789abcdef"}, {name: "sessions/telegram/a-1/files/x.txt", body: "hello"}}
	dest := t.TempDir()

	// the manifest says something else than the archive holds
	var m Manifest
	m.Version = manifestVersion
	m.Files = []FileEntry{{Path: "web_secret", Size: 16, SHA256: strings.Repeat("0", 64)}}
	if _, err := Restore(buildArchive(t, good[:1], &m), dest, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("a changed file: %v", err)
	}
	// a file missing from the archive
	m.Files = []FileEntry{{Path: "web_secret", Size: 16, SHA256: strings.Repeat("0", 64)}, {Path: "knowledge.db", Size: 1, SHA256: "00"}}
	if _, err := Restore(buildArchive(t, nil, &m), dest, RestoreOptions{}); err == nil {
		t.Error("a listed file that is absent")
	}
	// a file the manifest does not know
	m = Manifest{Version: manifestVersion}
	if _, err := Restore(buildArchive(t, good[:1], &m), dest, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "not in its manifest") {
		t.Errorf("an unlisted file: %v", err)
	}
	// no manifest at all (an interrupted backup)
	if _, err := Restore(buildArchive(t, good, &Manifest{Version: -1}), dest, RestoreOptions{}); err == nil || !strings.Contains(err.Error(), "no manifest") {
		t.Errorf("no manifest: %v", err)
	}
	// an unsupported version
	if _, err := Restore(buildArchive(t, good, &Manifest{Version: 99}), dest, RestoreOptions{}); err == nil {
		t.Error("a future version")
	}
	// not an archive, and a truncated one
	junk := filepath.Join(t.TempDir(), "x.tar.gz")
	_ = os.WriteFile(junk, []byte("this is not gzip"), 0o600)
	if _, err := Restore(junk, dest, RestoreOptions{}); err == nil {
		t.Error("junk accepted")
	}
	okp := buildArchive(t, good, nil)
	b, _ := os.ReadFile(okp)
	cut := filepath.Join(t.TempDir(), "cut.tar.gz")
	_ = os.WriteFile(cut, b[:len(b)/2], 0o600)
	if _, err := Restore(cut, dest, RestoreOptions{}); err == nil {
		t.Error("a truncated archive was accepted")
	}
	// a database that is not one
	bad := buildArchive(t, []entry{{name: "knowledge.db", body: "not a database at all, just text that is long enough to look like a header....."}}, nil)
	if _, err := Restore(bad, dest, RestoreOptions{}); err == nil {
		t.Error("a corrupt database was restored")
	}
	// the size cap
	if _, err := Restore(okp, dest, RestoreOptions{MaxBytes: 10}); err == nil || !strings.Contains(err.Error(), "allowed size") {
		t.Errorf("size cap: %v", err)
	}
	if ents, _ := os.ReadDir(dest); len(ents) != 0 {
		t.Errorf("leftovers: %v", ents)
	}
	// and the good one still works
	if _, err := Restore(okp, dest, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPruneAndLatest(t *testing.T) {
	dir := t.TempDir()
	for i := 1; i <= 5; i++ {
		write(t, filepath.Join(dir, Prefix+time.Date(2026, 1, i, 0, 0, 0, 0, time.UTC).Format(timeLayout)+Suffix), "x")
	}
	write(t, filepath.Join(dir, "notes.txt"), "keep me")
	write(t, filepath.Join(dir, Prefix+"20260101T000000Z"+Suffix+".partial"), "partial")
	if got := Latest(dir); !got.Equal(time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("latest = %v", got)
	}
	n, err := Prune(dir, 2)
	if err != nil || n != 3 {
		t.Fatalf("%d %v", n, err)
	}
	list, _ := List(dir)
	if len(list) != 2 || !strings.Contains(list[0], "20260104") || !strings.Contains(list[1], "20260105") {
		t.Errorf("kept %v", list)
	}
	for _, keep := range []string{"notes.txt", Prefix + "20260101T000000Z" + Suffix + ".partial"} {
		if _, err := os.Stat(filepath.Join(dir, keep)); err != nil {
			t.Errorf("%s was deleted", keep)
		}
	}
	if n, _ := Prune(dir, 5); n != 0 {
		t.Error("pruned below the limit")
	}
	if !Latest(t.TempDir()).IsZero() || func() bool { l, err := List(filepath.Join(dir, "missing")); return l != nil || err != nil }() {
		t.Error("an empty or missing directory")
	}
}

func TestCreateErrors(t *testing.T) {
	if _, err := Create(bg, t.TempDir(), Options{DataDir: filepath.Join(t.TempDir(), "nope")}); err == nil {
		t.Error("a missing data directory")
	}
	// an empty data directory still gives a valid (empty) backup
	out := t.TempDir()
	p, err := Create(bg, out, Options{DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if man, err := Verify(p, t.TempDir()); err != nil || len(man.Files) != 0 {
		t.Errorf("%v %+v", err, man)
	}
	// a cancelled backup leaves nothing
	d, _ := dataDir(t)
	ctx, cancel := context.WithCancel(bg)
	cancel()
	out2 := t.TempDir()
	if _, err := Create(ctx, out2, Options{DataDir: d}); err == nil {
		t.Error("a cancelled backup succeeded")
	}
	if ents, _ := os.ReadDir(out2); len(ents) != 0 {
		t.Errorf("leftovers: %v", ents)
	}
	// a second backup in the same second does not overwrite the first
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	if _, err := Create(bg, out2, Options{DataDir: d, Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(bg, out2, Options{DataDir: d, Now: now}); err == nil {
		t.Error("an existing backup was overwritten")
	}
}

func TestKnowledgeDatabaseElsewhere(t *testing.T) {
	d, _ := dataDir(t)
	elsewhere := filepath.Join(t.TempDir(), "kb", "my.db")
	mkdb(t, elsewhere, "far away")
	_ = os.Remove(filepath.Join(d, "knowledge.db"))
	p, err := Create(bg, t.TempDir(), Options{DataDir: d, KnowledgeDB: elsewhere})
	if err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	if _, err := Restore(p, dest, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if values(t, filepath.Join(dest, "knowledge.db")) != "far away" {
		t.Error("the knowledge database was not taken from its own location")
	}
}

func TestTriggersAreBackedUpAndRestored(t *testing.T) {
	d, _ := dataDir(t)
	mkdb(t, filepath.Join(d, "triggers.db"), "remind Peter")
	p, err := Create(bg, t.TempDir(), Options{DataDir: d})
	if err != nil {
		t.Fatal(err)
	}
	man, err := Verify(p, t.TempDir())
	if err != nil {
		t.Fatalf("an archive with triggers must verify: %v", err)
	}
	found := false
	for _, f := range man.Files {
		found = found || f.Path == "triggers.db"
	}
	if !found {
		t.Errorf("triggers.db is not in the backup: %+v", man.Files)
	}
	dest := t.TempDir()
	if _, err := Restore(p, dest, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := values(t, filepath.Join(dest, "triggers.db")); got != "remind Peter" {
		t.Errorf("restored triggers = %q", got)
	}
	// restoring over an existing one needs --force, and moves the old one aside
	if _, err := Restore(p, dest, RestoreOptions{}); err == nil {
		t.Error("an existing data directory must be refused without force")
	}
}
