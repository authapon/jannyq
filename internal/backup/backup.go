// Package backup makes and restores consistent copies of the bot's data: the
// chat databases and their files, the knowledge database, the web session key
// and the command audit log. SQLite databases are copied with VACUUM INTO, which
// takes a consistent snapshot while the bot keeps running.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

const (
	// Prefix and Suffix frame the names of backup files.
	Prefix = "jannyq-backup-"
	Suffix = ".tar.gz"

	manifestName    = "manifest.json"
	manifestVersion = 1
	timeLayout      = "20060102T150405Z"
)

// Options say what to back up.
type Options struct {
	// DataDir is the bot's data directory.
	DataDir string
	// KnowledgeDB is the knowledge database; it is stored as knowledge.db.
	// Empty means <DataDir>/knowledge.db when that exists.
	KnowledgeDB string
	// Files includes the files users sent (they can be large).
	Files bool
	// Version is recorded in the manifest.
	Version string
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// Manifest describes a backup; it is the last entry of the archive.
type Manifest struct {
	Version     int         `json:"version"`
	Created     time.Time   `json:"created"`
	JannyqVer   string      `json:"jannyq_version,omitempty"`
	IncludesFil bool        `json:"includes_files"`
	Files       []FileEntry `json:"files"`
}

// FileEntry lists one file of the archive.
type FileEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Create writes a backup into dir and returns its path. The file appears under
// its final name only when it is complete.
func Create(ctx context.Context, dir string, o Options) (string, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if fi, err := os.Stat(o.DataDir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("backup: %q is not a directory", o.DataDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	tmpDir, err := os.MkdirTemp(dir, ".work-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	created := o.Now().UTC()
	final := filepath.Join(dir, Prefix+created.Format(timeLayout)+Suffix)
	partial := final + ".partial"
	if _, err := os.Stat(final); err == nil {
		return "", fmt.Errorf("backup: %s already exists", final)
	}
	out, err := os.OpenFile(partial, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		if !ok {
			out.Close()
			os.Remove(partial)
		}
	}()
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	man := Manifest{Version: manifestVersion, Created: created, JannyqVer: o.Version, IncludesFil: o.Files}

	add := func(name, src string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		hdr := &tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(tw, h), io.LimitReader(f, fi.Size()))
		if err != nil {
			return err
		}
		if n != fi.Size() { // truncated while being read
			return fmt.Errorf("%s changed while it was being copied", name)
		}
		man.Files = append(man.Files, FileEntry{Path: name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))})
		return nil
	}
	snapshot := func(name, src string) error {
		tmp := filepath.Join(tmpDir, fmt.Sprintf("snap-%d.db", len(man.Files)))
		if err := vacuumInto(ctx, src, tmp); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		defer os.Remove(tmp)
		return add(name, tmp)
	}

	// chats
	sessions := filepath.Join(o.DataDir, "sessions")
	if _, err := os.Stat(sessions); err == nil {
		err = filepath.WalkDir(sessions, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil // deleted meanwhile
				}
				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			rel, _ := filepath.Rel(o.DataDir, p)
			name := filepath.ToSlash(rel)
			switch {
			case filepath.Base(p) == "session.db":
				return skipVanished(snapshot(name, p))
			case strings.HasSuffix(p, ".db-wal") || strings.HasSuffix(p, ".db-shm"):
				return nil // the snapshot already holds what the log contained
			case o.Files && strings.Contains(name, "/files/"):
				return skipVanished(add(name, p))
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	// knowledge base
	kdb := o.KnowledgeDB
	if kdb == "" {
		kdb = filepath.Join(o.DataDir, "knowledge.db")
	}
	if _, err := os.Stat(kdb); err == nil {
		if err := snapshot("knowledge.db", kdb); err != nil {
			return "", err
		}
	}
	// scheduled reminders and tasks
	if tdb := filepath.Join(o.DataDir, "triggers.db"); fileExists(tdb) {
		if err := snapshot("triggers.db", tdb); err != nil {
			return "", err
		}
	}
	// secrets and logs
	if err := skipVanished(add("web_secret", filepath.Join(o.DataDir, "web_secret"))); err != nil {
		return "", err
	}
	if entries, err := os.ReadDir(filepath.Join(o.DataDir, "audit")); err == nil {
		for _, e := range entries {
			if e.Type().IsRegular() {
				if err := skipVanished(add("audit/"+e.Name(), filepath.Join(o.DataDir, "audit", e.Name()))); err != nil {
					return "", err
				}
			}
		}
	}

	sort.Slice(man.Files, func(i, j int) bool { return man.Files[i].Path < man.Files[j].Path })
	mj, _ := json.MarshalIndent(man, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o600, Size: int64(len(mj)), ModTime: created, Typeflag: tar.TypeReg}); err != nil {
		return "", err
	}
	if _, err := tw.Write(mj); err != nil {
		return "", err
	}
	if err := tw.Close(); err != nil {
		return "", err
	}
	if err := gz.Close(); err != nil {
		return "", err
	}
	if err := out.Sync(); err != nil {
		return "", err
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(partial, final); err != nil {
		return "", err
	}
	ok = true
	return final, nil
}

func skipVanished(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// vacuumInto writes a consistent copy of the SQLite database src to dst.
func vacuumInto(ctx context.Context, src, dst string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(src)+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "VACUUM INTO '"+strings.ReplaceAll(filepath.ToSlash(dst), "'", "''")+"'")
	return err
}

// List returns the backups in dir, oldest first.
func List(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasPrefix(e.Name(), Prefix) && strings.HasSuffix(e.Name(), Suffix) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out) // the names hold the time, so this is chronological
	return out, nil
}

// Prune deletes all but the newest keep backups of dir and returns how many it deleted.
func Prune(dir string, keep int) (int, error) {
	list, err := List(dir)
	if err != nil || len(list) <= keep {
		return 0, err
	}
	n := 0
	for _, p := range list[:len(list)-keep] {
		if err := os.Remove(p); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// Latest returns the time of the newest backup in dir (zero if there is none).
func Latest(dir string) time.Time {
	list, _ := List(dir)
	if len(list) == 0 {
		return time.Time{}
	}
	name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(list[len(list)-1]), Prefix), Suffix)
	if t, err := time.Parse(timeLayout, name); err == nil {
		return t
	}
	return time.Time{}
}

// --- restore ---

// RestoreOptions control Restore.
type RestoreOptions struct {
	// Force replaces existing data, which is moved aside first, not deleted.
	Force bool
	// MaxBytes bounds the unpacked size (default 256 GiB).
	MaxBytes int64
	Now      func() time.Time
}

// allowedPath says whether an archive entry may be unpacked.
func allowedPath(name string) bool {
	if name == "" || len(name) > 400 || strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || path.Clean(name) != name {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	switch {
	case name == manifestName, name == "knowledge.db", name == "triggers.db", name == "web_secret":
		return true
	case strings.HasPrefix(name, "sessions/"), strings.HasPrefix(name, "audit/"):
		return strings.Count(name, "/") <= 6
	}
	return false
}

// stage unpacks and checks an archive into a fresh directory inside parent.
// Nothing outside that directory is written, whatever the archive contains.
func stage(archive, parent string, maxBytes int64) (dir string, man Manifest, err error) {
	if maxBytes <= 0 {
		maxBytes = 256 << 30
	}
	f, err := os.Open(archive)
	if err != nil {
		return "", man, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", man, fmt.Errorf("backup: not a backup archive: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return "", man, err
	}
	dir, err = os.MkdirTemp(parent, ".restore-")
	if err != nil {
		return "", man, err
	}
	staging := dir // `return "", ...` below clears dir, so the cleanup needs its own copy
	defer func() {
		if err != nil {
			os.RemoveAll(staging)
		}
	}()

	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	sums := map[string]FileEntry{}
	var total int64
	var manifest []byte
	for {
		hdr, herr := tr.Next()
		if herr == io.EOF {
			break
		}
		if herr != nil {
			return "", man, fmt.Errorf("backup: the archive is damaged: %w", herr)
		}
		if hdr.Typeflag == tar.TypeDir {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return "", man, fmt.Errorf("backup: refusing %q: only plain files are allowed", hdr.Name)
		}
		if !allowedPath(hdr.Name) {
			return "", man, fmt.Errorf("backup: refusing the path %q", hdr.Name)
		}
		if seen[hdr.Name] {
			return "", man, fmt.Errorf("backup: %q appears twice", hdr.Name)
		}
		seen[hdr.Name] = true
		if hdr.Size < 0 || total+hdr.Size > maxBytes {
			return "", man, errors.New("backup: the archive unpacks to more than the allowed size")
		}
		total += hdr.Size
		if hdr.Name == manifestName {
			if hdr.Size > 64<<20 {
				return "", man, errors.New("backup: the manifest is too large")
			}
			if manifest, err = io.ReadAll(io.LimitReader(tr, hdr.Size)); err != nil {
				return "", man, err
			}
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(hdr.Name))
		if err = os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
			return "", man, err
		}
		out, oerr := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if oerr != nil {
			return "", man, oerr
		}
		h := sha256.New()
		n, cerr := io.Copy(io.MultiWriter(out, h), io.LimitReader(tr, hdr.Size))
		if cerr == nil {
			cerr = out.Close()
		} else {
			out.Close()
		}
		if cerr != nil {
			return "", man, fmt.Errorf("backup: the archive is damaged: %w", cerr)
		}
		if n != hdr.Size {
			return "", man, fmt.Errorf("backup: %q is cut short", hdr.Name)
		}
		sums[hdr.Name] = FileEntry{Path: hdr.Name, Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	}
	if manifest == nil {
		return "", man, errors.New("backup: the archive has no manifest (it is incomplete)")
	}
	if err = json.Unmarshal(manifest, &man); err != nil || man.Version != manifestVersion {
		return "", man, fmt.Errorf("backup: unreadable manifest or unsupported version %d", man.Version)
	}
	listed := map[string]bool{}
	for _, e := range man.Files {
		got, ok := sums[e.Path]
		if !ok || got.Size != e.Size || got.SHA256 != e.SHA256 {
			err = fmt.Errorf("backup: %q does not match the manifest: the archive is damaged or was changed", e.Path)
			return "", man, err
		}
		listed[e.Path] = true
	}
	for p := range sums {
		if !listed[p] {
			err = fmt.Errorf("backup: %q is in the archive but not in its manifest", p)
			return "", man, err
		}
	}
	return dir, man, nil
}

// Verify unpacks the archive into a scratch directory under tmpParent and
// checks it completely, then deletes the scratch directory.
func Verify(archive, tmpParent string) (Manifest, error) {
	dir, man, err := stage(archive, tmpParent, 0)
	if err != nil {
		return man, err
	}
	defer os.RemoveAll(dir)
	// the databases must be sound too
	for _, e := range man.Files {
		if strings.HasSuffix(e.Path, ".db") {
			if err := integrity(filepath.Join(dir, filepath.FromSlash(e.Path))); err != nil {
				return man, fmt.Errorf("backup: %s: %w", e.Path, err)
			}
		}
	}
	return man, nil
}

func integrity(p string) error {
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(p)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return fmt.Errorf("database integrity check failed: %s", res)
	}
	return nil
}

// Restore unpacks a backup into dataDir. Without Force it refuses when the
// directory already holds chats or a knowledge base. Stop the bot first.
func Restore(archive, dataDir string, o RestoreOptions) (Manifest, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	dir, man, err := stage(archive, dataDir, o.MaxBytes)
	if err != nil {
		return man, err
	}
	defer os.RemoveAll(dir)
	for _, e := range man.Files {
		if strings.HasSuffix(e.Path, ".db") {
			if err := integrity(filepath.Join(dir, filepath.FromSlash(e.Path))); err != nil {
				return man, fmt.Errorf("backup: %s: %w", e.Path, err)
			}
		}
	}
	tops := []string{"sessions", "knowledge.db", "triggers.db", "web_secret", "audit"}
	var existing []string
	for _, t := range tops {
		if _, err := os.Lstat(filepath.Join(dataDir, t)); err == nil {
			if _, inBackup := os.Lstat(filepath.Join(dir, t)); inBackup == nil {
				existing = append(existing, t)
			}
		}
	}
	if len(existing) > 0 && !o.Force {
		return man, fmt.Errorf("backup: %s already holds %s; use --force to move it aside and restore", dataDir, strings.Join(existing, ", "))
	}
	stamp := o.Now().UTC().Format(timeLayout)
	for _, t := range existing {
		if err := os.Rename(filepath.Join(dataDir, t), filepath.Join(dataDir, t+".before-restore-"+stamp)); err != nil {
			return man, err
		}
	}
	for _, t := range tops {
		src := filepath.Join(dir, t)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if err := os.Rename(src, filepath.Join(dataDir, t)); err != nil {
			return man, err
		}
	}
	return man, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
