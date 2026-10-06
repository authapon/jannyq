package web

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func drain(sub *subscriber) []event {
	var out []event
	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func TestHubFansOutAndAssignsIDs(t *testing.T) {
	h := newHub()
	a1, _, cancelA1, err := h.subscribe("chat-a", "1.1.1.1", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancelA1()
	a2, _, cancelA2, _ := h.subscribe("chat-a", "1.1.1.1", 0)
	defer cancelA2()
	b, _, cancelB, _ := h.subscribe("chat-b", "2.2.2.2", 0)
	defer cancelB()

	h.publish("chat-a", "typing", map[string]bool{"typing": true})
	h.publish("chat-a", "message", map[string]string{"text": "hello"})
	h.publish("chat-a", "message", map[string]string{"text": "again"})

	for _, sub := range []*subscriber{a1, a2} {
		evs := drain(sub)
		if len(evs) != 3 || evs[0].Type != "typing" || evs[0].ID != 0 || evs[1].ID != 1 || evs[2].ID != 2 {
			t.Errorf("events = %+v", evs)
		}
		if string(evs[1].Data) != `{"text":"hello"}` {
			t.Errorf("data = %s", evs[1].Data)
		}
	}
	if evs := drain(b); len(evs) != 0 {
		t.Errorf("chat-b received chat-a's events: %+v", evs)
	}
}

func TestHubReplayOnlyWhenResuming(t *testing.T) {
	h := newHub()
	for i := 1; i <= 5; i++ {
		h.publish("c", "message", map[string]int{"n": i}) // nobody is listening
	}
	_, replay, cancel, _ := h.subscribe("c", "ip", 0)
	cancel()
	if len(replay) != 0 {
		t.Errorf("a fresh page load must not replay: %+v", replay)
	}
	_, replay, cancel, _ = h.subscribe("c", "ip", 3)
	defer cancel()
	if len(replay) != 2 || replay[0].ID != 4 || replay[1].ID != 5 {
		t.Errorf("replay after id 3 = %+v", replay)
	}
}

func TestHubOutboxIsBounded(t *testing.T) {
	h := newHub()
	for i := 0; i < outboxSize*3; i++ {
		h.publish("c", "message", map[string]string{"text": strconv.Itoa(i)})
	}
	h.mu.Lock()
	n := len(h.sessions["c"].outbox)
	h.mu.Unlock()
	if n != outboxSize {
		t.Errorf("outbox holds %d events, want %d", n, outboxSize)
	}
	_, replay, cancel, _ := h.subscribe("c", "ip", 1)
	defer cancel()
	if len(replay) != outboxSize || replay[len(replay)-1].ID != int64(outboxSize*3) {
		t.Errorf("replay has %d events, last id %d", len(replay), replay[len(replay)-1].ID)
	}
}

func TestHubDropsSlowSubscribers(t *testing.T) {
	h := newHub()
	slow, _, cancel, _ := h.subscribe("c", "ip", 0)
	defer cancel()
	for i := 0; i < subscriberBuffer+5; i++ {
		h.publish("c", "message", map[string]int{"n": i}) // nobody reads from slow
	}
	if h.openStreams() != 0 {
		t.Errorf("slow subscriber still counted: %d", h.openStreams())
	}
	got := drain(slow)
	if len(got) != subscriberBuffer { // channel was closed after its buffer filled
		t.Errorf("slow subscriber got %d buffered events", len(got))
	}
	cancel() // cancelling a dropped subscriber is harmless
	// ...and the client can catch up from the outbox
	_, replay, c2, _ := h.subscribe("c", "ip", int64(subscriberBuffer))
	defer c2()
	if len(replay) != 5 {
		t.Errorf("replay = %d events, want 5", len(replay))
	}
}

func TestHubLimits(t *testing.T) {
	h := newHub()
	var cancels []func()
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()
	for i := 0; i < maxSubsPerSession; i++ {
		_, _, c, err := h.subscribe("same-chat", "ip"+strconv.Itoa(i), 0)
		if err != nil {
			t.Fatalf("stream %d refused: %v", i, err)
		}
		cancels = append(cancels, c)
	}
	if _, _, _, err := h.subscribe("same-chat", "other", 0); err != errTooManyStreams {
		t.Errorf("per-chat limit: err = %v", err)
	}
	for i := 0; i < maxSubsPerIP; i++ {
		_, _, c, err := h.subscribe("chat"+strconv.Itoa(i), "busy-ip", 0)
		if err != nil {
			t.Fatalf("stream %d from one IP refused: %v", i, err)
		}
		cancels = append(cancels, c)
	}
	if _, _, _, err := h.subscribe("another", "busy-ip", 0); err != errTooManyStreams {
		t.Errorf("per-IP limit: err = %v", err)
	}
	if _, _, c, err := h.subscribe("another", "fresh-ip", 0); err != nil {
		t.Errorf("another IP must still be served: %v", err)
	} else {
		c()
	}
}

func TestHubForgetsIdleChats(t *testing.T) {
	h := newHub()
	now := time.Unix(10_000, 0)
	h.now = func() time.Time { return now }
	h.publish("idle", "message", map[string]string{"text": "x"})
	_, _, cancel, _ := h.subscribe("watched", "ip", 0)
	defer cancel()
	now = now.Add(idleSessionTTL + 2*time.Minute)
	h.publish("trigger", "message", map[string]string{"text": "x"}) // gc runs on use
	h.mu.Lock()
	_, idle := h.sessions["idle"]
	_, watched := h.sessions["watched"]
	h.mu.Unlock()
	if idle || !watched {
		t.Errorf("idle chat kept: %v, chat with an open stream kept: %v", idle, watched)
	}
}

func TestCookieSigner(t *testing.T) {
	s := newSigner([]byte("0123456789abcdef"), "")
	v, id := s.issue()
	if s.verify(v) != id || len(id) != 32 {
		t.Fatalf("round trip failed: %q → %q", v, s.verify(v))
	}
	v2, id2 := s.issue()
	if id == id2 || v == v2 {
		t.Error("ids must be random")
	}
	// flip returns a character different from c, so a "tampered" value always differs.
	flip := func(c byte) string {
		if c == 'a' {
			return "b"
		}
		return "a"
	}
	for name, bad := range map[string]string{
		"empty":        "",
		"no dot":       id,
		"tampered id":  flip(id[0]) + id[1:] + v[len(id):],
		"tampered sig": v[:len(v)-1] + flip(v[len(v)-1]),
		"other id":     id2 + v[len(id):],
		"short id":     id[:8] + v[len(id):],
		"non-hex id":   "zz" + id[2:] + v[len(id):],
		"extra suffix": v + ".x",
		"unsigned":     id + ".",
	} {
		if s.verify(bad) != "" {
			t.Errorf("%s accepted: %q", name, bad)
		}
	}
	if newSigner([]byte("another-secret-xx"), "").verify(v) != "" {
		t.Error("a cookie must not verify under another secret")
	}
	if newSigner([]byte("0123456789abcdef"), "code-2").verify(v) != "" {
		t.Error("a cookie must not verify after the access code changed")
	}
	if newSigner([]byte("0123456789abcdef"), "").verify(v) != id {
		t.Error("the same secret and code must verify")
	}
}

func TestHubOutboxIsBoundedInBytesToo(t *testing.T) {
	h := newHub()
	big := strings.Repeat("a", 40<<10)
	for i := 0; i < 10; i++ {
		h.publish("c", "message", map[string]string{"text": big})
	}
	h.mu.Lock()
	s := h.sessions["c"]
	n, bytes := len(s.outbox), s.outboxBytes
	last := s.outbox[len(s.outbox)-1].ID
	h.mu.Unlock()
	if bytes > outboxMaxBytes+(41<<10) || n > 4 || last != 10 {
		t.Errorf("outbox holds %d events / %d bytes (last id %d)", n, bytes, last)
	}
	// a single huge reply is still delivered to the live stream and kept alone
	h.publish("c2", "message", map[string]string{"text": strings.Repeat("a", 300<<10)})
	h.mu.Lock()
	n2 := len(h.sessions["c2"].outbox)
	h.mu.Unlock()
	if n2 != 1 {
		t.Errorf("the newest event was dropped: %d kept", n2)
	}
}

func TestHubEvictsTheIdlestChatInsteadOfRefusingNewOnes(t *testing.T) {
	h := newHub()
	now := time.Unix(50_000, 0)
	h.now = func() time.Time { now = now.Add(time.Second); return now }
	for i := 0; i < maxHubSessions; i++ {
		h.publish("chat"+strconv.Itoa(i), "message", map[string]int{"n": i})
	}
	_, _, watch, err := h.subscribe("chat0", "ip", 0) // the oldest chat has an open stream: not evictable
	if err != nil {
		t.Fatal(err)
	}
	defer watch()
	h.publish("newcomer", "message", map[string]string{"text": "hi"})
	_, replay, cancel, err := h.subscribe("newcomer", "ip2", 0)
	if err != nil {
		t.Fatalf("a new chat was refused although idle chats could be evicted: %v", err)
	}
	cancel()
	_ = replay
	h.mu.Lock()
	_, kept := h.sessions["chat0"]
	_, evicted := h.sessions["chat1"] // the idlest one without a stream
	total := len(h.sessions)
	h.mu.Unlock()
	if !kept || evicted || total > maxHubSessions {
		t.Errorf("chat0 kept=%v, chat1 still present=%v, total=%d", kept, evicted, total)
	}
}
