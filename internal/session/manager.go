package session

import (
	"context"
	"errors"
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
	s        *Session
	refs     int // running + waiting requests; guarded by Manager.mu
	lastUsed time.Time
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

	if e.s == nil {
		s, err := openSession(channel, chatID, sessionDir(m.dir, channel, chatID))
		if err != nil {
			return err
		}
		e.s = s
	}
	return fn(e.s)
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
