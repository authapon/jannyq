// Package skill loads "skills" from a directory. A skill is a folder holding
// a SKILL.md file with a short description in YAML front matter, followed by
// instructions for the model. Only the descriptions go into the system
// prompt; the full instructions are loaded on demand with the load_skill tool.
package skill

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const (
	skillFile   = "SKILL.md"
	maxFileSize = 64 << 10
	maxListed   = 50
	rescanAfter = 10 * time.Second
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// Summary is what the system prompt shows for a skill.
type Summary struct {
	Name        string
	Description string
}

// Store reads skills from a directory and notices changes.
type Store struct {
	dir         string
	sandboxPath string
	log         *slog.Logger

	mu        sync.Mutex
	skills    map[string]Summary
	scannedAt time.Time
}

// NewStore opens dir. sandboxPath is where the same directory is mounted
// inside the command sandbox ("" if it is not), so instructions can point at
// scripts the model may run.
func NewStore(dir, sandboxPath string, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("skills: %s is not a directory", dir)
	}
	s := &Store{dir: dir, sandboxPath: strings.TrimRight(sandboxPath, "/"), log: log}
	s.refresh()
	return s, nil
}

// Summaries lists the available skills by name, rescanning when stale.
func (s *Store) Summaries() []Summary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if time.Since(s.scannedAt) > rescanAfter {
		s.scanLocked()
	}
	return s.sortedLocked()
}

func (s *Store) refresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scanLocked()
}

func (s *Store) sortedLocked() []Summary {
	out := make([]Summary, 0, len(s.skills))
	for _, sk := range s.skills {
		out = append(out, sk)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type frontMatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// parse splits SKILL.md into front matter and body.
func parse(data []byte) (frontMatter, string, error) {
	var fm frontMatter
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return fm, "", errors.New("missing --- front matter")
	}
	rest := text[4:]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return fm, "", errors.New("front matter is not closed with ---")
	}
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		return fm, "", fmt.Errorf("invalid front matter: %w", err)
	}
	body := rest[end+4:]
	if i := strings.IndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	} else {
		body = ""
	}
	fm.Description = strings.Join(strings.Fields(fm.Description), " ")
	return fm, strings.TrimSpace(body), nil
}

func (s *Store) scanLocked() {
	s.scannedAt = time.Now()
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		s.log.Warn("cannot read the skills directory", "dir", s.dir, "err", err)
		return
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		s.log.Warn("cannot list the skills directory", "dir", s.dir, "err", err)
		return
	}
	found := map[string]Summary{}
	for _, ent := range entries {
		name := ent.Name()
		if !ent.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !nameRe.MatchString(name) {
			s.warnOnce("skipping skill: folder name must be lowercase letters, digits, - or _", name)
			continue
		}
		f, err := root.Open(path.Join(name, skillFile))
		if err != nil {
			continue // not a skill folder
		}
		data, err := io.ReadAll(io.LimitReader(f, maxFileSize))
		f.Close()
		if err != nil {
			continue
		}
		fm, _, err := parse(data)
		switch {
		case err != nil:
			s.warnOnce("skipping skill: "+err.Error(), name)
		case fm.Description == "":
			s.warnOnce("skipping skill: the description in the front matter is required", name)
		default:
			if fm.Name != "" && fm.Name != name {
				s.warnOnce(fmt.Sprintf("front matter name %q differs from the folder name; using the folder name", fm.Name), name)
			}
			found[name] = Summary{Name: name, Description: fm.Description}
		}
	}
	s.skills = found
}

var warned sync.Map

// warnOnce avoids repeating the same complaint on every rescan.
func (s *Store) warnOnce(msg, name string) {
	if _, dup := warned.LoadOrStore(s.dir+"\x00"+name+"\x00"+msg, true); !dup {
		s.log.Warn(msg, "skill", name)
	}
}

// ErrNotFound is returned for unknown skills and files.
var ErrNotFound = errors.New("skill or file not found")

// Load returns a skill's instructions, or when file is not empty, that file
// from the skill's folder.
func (s *Store) Load(name, file string) (string, error) {
	s.mu.Lock()
	if _, ok := s.skills[name]; !ok {
		s.scanLocked() // maybe it was added since the last scan
	}
	_, ok := s.skills[name]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("%w: no skill named %q", ErrNotFound, name)
	}

	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return "", err
	}
	defer root.Close()

	if file != "" {
		return s.loadFile(root, name, file)
	}
	f, err := root.Open(path.Join(name, skillFile))
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return "", err
	}
	_, body, err := parse(data)
	if err != nil {
		return "", err
	}
	if len(data) > maxFileSize {
		body += "\n\n[truncated: the instructions are longer than 64 KB]"
	}

	var sb strings.Builder
	sb.WriteString(body)
	sb.WriteString("\n\n---\n")
	if s.sandboxPath != "" {
		fmt.Fprintf(&sb, "Skill folder inside the sandbox (read-only): %s/%s\n", s.sandboxPath, name)
	}
	if files := s.listFiles(root, name); len(files) > 0 {
		sb.WriteString("Other files in this skill (read one with load_skill and the file argument):\n")
		for _, f := range files {
			sb.WriteString("- " + f + "\n")
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

func (s *Store) loadFile(root *os.Root, name, file string) (string, error) {
	clean := path.Clean(strings.ReplaceAll(file, "\\", "/"))
	if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return "", errors.New("file must be a relative path inside the skill folder")
	}
	f, err := root.Open(path.Join(name, clean)) // refuses to leave the skills directory
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil || fi.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrNotFound, clean)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileSize+1))
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data[:min(len(data), maxFileSize)]) || bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("this is a binary file; run it with run_command instead of reading it")
	}
	text := string(data)
	if len(data) > maxFileSize {
		text = string(data[:maxFileSize]) + "\n\n[truncated at 64 KB]"
	}
	return text, nil
}

// listFiles returns the skill's files other than SKILL.md, relative to it.
func (s *Store) listFiles(root *os.Root, name string) []string {
	sub, err := root.OpenRoot(name)
	if err != nil {
		return nil
	}
	defer sub.Close()
	var files []string
	_ = fs.WalkDir(sub.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && p != "." {
				return fs.SkipDir
			}
			return nil
		}
		if p == skillFile || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		if files = append(files, p); len(files) >= maxListed {
			return fs.SkipAll
		}
		return nil
	})
	sort.Strings(files)
	return files
}
