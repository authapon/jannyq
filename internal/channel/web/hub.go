package web

import (
	"encoding/json"
	"errors"
	"sync"
	"time"
)

const (
	outboxSize        = 50 // events kept per chat for clients that reconnect
	outboxMaxBytes    = 128 << 10
	subscriberBuffer  = 32
	idleSessionTTL    = 30 * time.Minute
	maxSubsPerSession = 4
	maxSubsPerIP      = 12
	maxSubsTotal      = 500
	maxHubSessions    = 5000
)

// Errors returned by subscribe.
var (
	errTooManyStreams = errors.New("web: too many open streams")
	errHubFull        = errors.New("web: too many active chats")
)

// event is one server-sent event. Typing events carry no ID and are not kept.
type event struct {
	ID   int64
	Type string
	Data []byte
}

type subscriber struct {
	ch chan event
	ip string
}

type hubSession struct {
	nextID      int64
	outbox      []event
	outboxBytes int
	subs        map[*subscriber]struct{}
	lastActive  time.Time
}

// hub fans events out to the open streams of each chat. Publishing never
// blocks: a client that cannot keep up is disconnected and catches up from the
// outbox when it reconnects.
type hub struct {
	now func() time.Time

	mu        sync.Mutex
	sessions  map[string]*hubSession
	totalSubs int
	ipSubs    map[string]int
	lastGC    time.Time
}

func newHub() *hub {
	return &hub{now: time.Now, sessions: map[string]*hubSession{}, ipSubs: map[string]int{}}
}

func (h *hub) sessionLocked(id string) (*hubSession, error) {
	s := h.sessions[id]
	if s == nil {
		if len(h.sessions) >= maxHubSessions && !h.evictLocked() {
			return nil, errHubFull
		}
		s = &hubSession{subs: map[*subscriber]struct{}{}}
		h.sessions[id] = s
	}
	s.lastActive = h.now()
	return s, nil
}

// publish sends an event to every stream of the chat. Events of type
// "message" get an ID and are kept for replay.
func (h *hub) publish(id, typ string, payload any) {
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gcLocked()
	s, err := h.sessionLocked(id)
	if err != nil {
		return
	}
	ev := event{Type: typ, Data: data}
	if typ == "message" {
		s.nextID++
		ev.ID = s.nextID
		s.outbox = append(s.outbox, ev)
		s.outboxBytes += len(ev.Data)
		// Bounded by count and by size, but always keep the newest event.
		for len(s.outbox) > 1 && (len(s.outbox) > outboxSize || s.outboxBytes > outboxMaxBytes) {
			s.outboxBytes -= len(s.outbox[0].Data)
			s.outbox = s.outbox[1:]
		}
	}
	for sub := range s.subs {
		select {
		case sub.ch <- ev:
		default: // too slow: drop the stream, the client resumes with Last-Event-ID
			h.removeLocked(s, sub)
		}
	}
}

// subscribe opens a stream. Messages with an ID above lastID are returned for
// replay (lastID 0 means a fresh page load: nothing is replayed). cancel must
// be called when the stream ends.
func (h *hub) subscribe(id, ip string, lastID int64) (sub *subscriber, replay []event, cancel func(), err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.gcLocked()
	s, err := h.sessionLocked(id)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(s.subs) >= maxSubsPerSession || h.ipSubs[ip] >= maxSubsPerIP || h.totalSubs >= maxSubsTotal {
		return nil, nil, nil, errTooManyStreams
	}
	sub = &subscriber{ch: make(chan event, subscriberBuffer), ip: ip}
	s.subs[sub] = struct{}{}
	h.totalSubs++
	h.ipSubs[ip]++
	if lastID > 0 {
		for _, ev := range s.outbox {
			if ev.ID > lastID {
				replay = append(replay, ev)
			}
		}
	}
	cancel = func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if cur := h.sessions[id]; cur != nil {
			h.removeLocked(cur, sub)
		}
	}
	return sub, replay, cancel, nil
}

func (h *hub) removeLocked(s *hubSession, sub *subscriber) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	close(sub.ch)
	h.totalSubs--
	if h.ipSubs[sub.ip]--; h.ipSubs[sub.ip] <= 0 {
		delete(h.ipSubs, sub.ip)
	}
}

// evictLocked makes room for a new chat by forgetting the one that has been
// idle longest among those without an open stream.
func (h *hub) evictLocked() bool {
	var victim string
	var oldest time.Time
	for id, s := range h.sessions {
		if len(s.subs) == 0 && (victim == "" || s.lastActive.Before(oldest)) {
			victim, oldest = id, s.lastActive
		}
	}
	if victim == "" {
		return false
	}
	delete(h.sessions, victim)
	return true
}

// gcLocked forgets chats that have been idle without open streams.
func (h *hub) gcLocked() {
	now := h.now()
	if now.Sub(h.lastGC) < time.Minute {
		return
	}
	h.lastGC = now
	for id, s := range h.sessions {
		if len(s.subs) == 0 && now.Sub(s.lastActive) > idleSessionTTL {
			delete(h.sessions, id)
		}
	}
}

// openStreams reports the number of open streams.
func (h *hub) openStreams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.totalSubs
}
