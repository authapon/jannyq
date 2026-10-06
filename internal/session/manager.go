package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrBusy is returned when too many requests for one chat are already queued.
var ErrBusy = errors.New("session: too many pending requests for this chat")

// ErrClosed is returned after Close.
var ErrClosed = errors.New("session: manager closed")

// Manager opens per-chat sessions on demand, keeps a bounded number of
// databases open (least recently used are closed), and serialises work per
// chat so two messages of one chat never run concurrently.
type Manager struct {
	dir        string
	maxOpen    int
	maxPending int

	mu      sync.Mutex
	entries map[string]*entry
	closed  bool
}

type entry struct {
	sem      chan struct{} // capacity 1: held while a request runs
	openMu   sync.Mutex    // guards s while it is being opened
	s        *Session
	refs     int // running + waiting requests; guarded by Manager.mu
	lastUsed time.Time
}

// session opens the chat's database on first use.
func (e *entry) session(channel, chatID, base string) (*Session, error) {
	e.openMu.Lock()
	defer e.openMu.Unlock()
	if e.s == nil {
		s, err := openSession(channel, chatID, sessionDir(base, channel, chatID))
		if err != nil {
			return nil, err
		}
		e.s = s
	}
	return e.s, nil
}

// NewManager stores sessions under dir. maxOpen bounds simultaneously open
// databases; maxPending bounds queued requests per chat (including the
// running one).
func NewManager(dir string, maxOpen, maxPending int) *Manager {
	if maxOpen < 1 {
		maxOpen = 64
	}
	if maxPending < 1 {
		maxPending = 4
	}
	return &Manager{dir: dir, maxOpen: maxOpen, maxPending: maxPending, entries: map[string]*entry{}}
}

// With runs fn with exclusive access to the chat's session.
func (m *Manager) With(ctx context.Context, channel, chatID string, fn func(*Session) error) error {
	key := channel + "\x00" + chatID
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	e := m.entries[key]
	if e == nil {
		e = &entry{sem: make(chan struct{}, 1)}
		m.entries[key] = e
	}
	if e.refs >= m.maxPending {
		m.mu.Unlock()
		return ErrBusy
	}
	e.refs++
	m.mu.Unlock()

	defer m.release(e)
	select {
	case e.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-e.sem }()

	s, err := e.session(channel, chatID, m.dir)
	if err != nil {
		return err
	}
	return fn(s)
}

// Exists reports whether the chat has ever stored anything, without creating it.
func (m *Manager) Exists(channel, chatID string) bool {
	_, err := os.Stat(filepath.Join(sessionDir(m.dir, channel, chatID), "session.db"))
	return err == nil
}

// Peek runs fn with access to the chat's session without waiting for a
// running request or counting towards the queue limit. fn may only use
// methods that are safe to call alongside a running request: reads (Recent,
// Messages, Summary, Count) and AppendEntries.
func (m *Manager) Peek(channel, chatID string, fn func(*Session) error) error {
	key := channel + "\x00" + chatID
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrClosed
	}
	e := m.entries[key]
	if e == nil {
		e = &entry{sem: make(chan struct{}, 1)}
		m.entries[key] = e
	}
	e.refs++
	m.mu.Unlock()
	defer m.release(e)

	s, err := e.session(channel, chatID, m.dir)
	if err != nil {
		return err
	}
	return fn(s)
}

// Record is Peek under the name used when the point is to write: storing
// messages of a group conversation that nobody asked the bot to answer.
func (m *Manager) Record(channel, chatID string, fn func(*Session) error) error {
	return m.Peek(channel, chatID, fn)
}

func (m *Manager) release(e *entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.refs--
	e.lastUsed = time.Now()
	for len(m.entries) > m.maxOpen {
		var victimKey string
		var victim *entry
		for k, c := range m.entries {
			if c.refs == 0 && (victim == nil || c.lastUsed.Before(victim.lastUsed)) {
				victimKey, victim = k, c
			}
		}
		if victim == nil {
			return // everything is in use
		}
		if victim.s != nil {
			_ = victim.s.close()
		}
		delete(m.entries, victimKey)
	}
}

// Close closes all open sessions. Calls to With afterwards fail.
func (m *Manager) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	var first error
	for k, e := range m.entries {
		if e.s != nil && e.refs == 0 {
			if err := e.s.close(); err != nil && first == nil {
				first = err
			}
		}
		delete(m.entries, k)
	}
	return first
}

// OpenCount returns the number of chats whose database is open.
func (m *Manager) OpenCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.entries {
		if e.s != nil {
			n++
		}
	}
	return n
}

// lastActivity is when a chat's database was last written.
func lastActivity(dir string) (time.Time, bool) {
	var last time.Time
	found := false
	for _, name := range []string{"session.db", "session.db-wal"} {
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil {
			found = true
			if fi.ModTime().After(last) {
				last = fi.ModTime()
			}
		}
	}
	return last, found
}

// Sweep deletes the chats (database and files) that have not been written to
// for olderThan. Chats in use are never touched. It returns how many were
// deleted. Directories that are not chats (no session.db) are left alone.
func (m *Manager) Sweep(olderThan time.Duration, now time.Time) (int, error) {
	channels, err := os.ReadDir(m.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	deleted := 0
	for _, ch := range channels {
		if !ch.IsDir() {
			continue
		}
		chats, err := os.ReadDir(filepath.Join(m.dir, ch.Name()))
		if err != nil {
			continue
		}
		for _, c := range chats {
			dir := filepath.Join(m.dir, ch.Name(), c.Name())
			if !c.IsDir() {
				continue
			}
			last, ok := lastActivity(dir)
			if !ok || now.Sub(last) < olderThan {
				continue
			}
			if m.removeIdle(dir) {
				deleted++
			}
		}
	}
	return deleted, nil
}

// removeIdle deletes a chat directory unless the chat is in use. The lock is
// held throughout, so nobody can start using the chat in between.
func (m *Manager) removeIdle(dir string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	for key, e := range m.entries {
		ch, id, _ := strings.Cut(key, "\x00")
		if sessionDir(m.dir, ch, id) != dir {
			continue
		}
		if e.refs > 0 {
			return false
		}
		if e.s != nil {
			_ = e.s.close()
		}
		delete(m.entries, key)
	}
	return os.RemoveAll(dir) == nil
}
