// Package audit records every command the model runs, one JSON object per
// line, so that abuse can be traced to a chat and user.
package audit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Entry is one audited command.
type Entry struct {
	Time        time.Time `json:"time"`
	Channel     string    `json:"channel"`
	Chat        string    `json:"chat"`
	UserID      string    `json:"user_id"`
	UserName    string    `json:"user_name,omitempty"`
	Workspace   string    `json:"workspace,omitempty"`
	Command     string    `json:"command"`
	ExitCode    int       `json:"exit_code"`
	Signal      string    `json:"signal,omitempty"`
	TimedOut    bool      `json:"timed_out,omitempty"`
	DurationMS  int64     `json:"duration_ms"`
	OutputBytes int       `json:"output_bytes"`
	Error       string    `json:"error,omitempty"`
}

const (
	maxCommandRunes = 4000
	defaultMaxBytes = 10 << 20
	keepFiles       = 3
)

// Logger appends entries to a JSON Lines file, rotating it by size.
type Logger struct {
	path     string
	maxBytes int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

// Open creates the log file (and its directory) with owner-only access.
// maxBytes <= 0 selects 10 MiB per file; the last three rotated files are kept.
func Open(path string, maxBytes int64) (*Logger, error) {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	l := &Logger{path: path, maxBytes: maxBytes}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *Logger) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, fi.Size()
	return nil
}

// Log appends an entry. Failures are returned but must never block the
// command that was audited.
func (l *Logger) Log(e Entry) error {
	if l == nil {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if r := []rune(e.Command); len(r) > maxCommandRunes {
		e.Command = string(r[:maxCommandRunes]) + "…"
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return os.ErrClosed
	}
	if l.size+int64(len(b)) > l.maxBytes && l.size > 0 {
		if err := l.rotate(); err != nil {
			return err
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	return err
}

func (l *Logger) rotate() error {
	l.f.Close()
	l.f = nil
	for i := keepFiles - 1; i >= 1; i-- {
		_ = os.Rename(fmt.Sprintf("%s.%d", l.path, i), fmt.Sprintf("%s.%d", l.path, i+1))
	}
	if err := os.Rename(l.path, l.path+".1"); err != nil {
		return err
	}
	return l.open()
}

// Close closes the file.
func (l *Logger) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}
