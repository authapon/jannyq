package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/llm"
)

var ctx = context.Background()

func user(s string) llm.Message      { return llm.Message{Role: llm.RoleUser, Content: s} }
func assistant(s string) llm.Message { return llm.Message{Role: llm.RoleAssistant, Content: s} }
func call(id string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: id, Name: "t", Arguments: json.RawMessage(`{"a":1}`)}}}
}
func result(id string) llm.Message {
	return llm.Message{Role: llm.RoleTool, ToolCallID: id, Name: "t", Content: "r-" + id}
}

func withSession(t *testing.T, fn func(*Session)) {
	t.Helper()
	m := NewManager(t.TempDir(), 4, 4)
	defer m.Close()
	if err := m.With(ctx, "test", "chat1", func(s *Session) error { fn(s); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestAppendMessagesRoundTrip(t *testing.T) {
	withSession(t, func(s *Session) {
		if err := s.Append(ctx, user("สวัสดี"), call("c1"), result("c1"), assistant("done")); err != nil {
			t.Fatal(err)
		}
		msgs, err := s.Messages(ctx)
		if err != nil || len(msgs) != 4 {
			t.Fatalf("msgs=%d err=%v", len(msgs), err)
		}
		if msgs[0].Message.Content != "สวัสดี" {
			t.Errorf("content = %q", msgs[0].Message.Content)
		}
		tc := msgs[1].Message.ToolCalls
		if len(tc) != 1 || tc[0].ID != "c1" || string(tc[0].Arguments) != `{"a":1}` {
			t.Errorf("tool calls = %+v", tc)
		}
		if r := msgs[2].Message; r.ToolCallID != "c1" || r.Name != "t" || r.Content != "r-c1" {
			t.Errorf("tool result = %+v", r)
		}
		if n, _ := s.Count(ctx); n != 2 {
			t.Errorf("count = %d, want 2 (tool plumbing excluded)", n)
		}
	})
}

func TestCompactAndReset(t *testing.T) {
	withSession(t, func(s *Session) {
		_ = s.Append(ctx, user("a"), assistant("b"), user("c"), assistant("d"))
		msgs, _ := s.Messages(ctx)
		_ = s.SetLastPromptTokens(ctx, 123)
		if err := s.Compact(ctx, msgs[1].ID, "SUMMARY"); err != nil {
			t.Fatal(err)
		}
		left, _ := s.Messages(ctx)
		if len(left) != 2 || left[0].Message.Content != "c" {
			t.Errorf("left = %+v", left)
		}
		if sum, _ := s.Summary(ctx); sum != "SUMMARY" {
			t.Errorf("summary = %q", sum)
		}
		if s.LastPromptTokens(ctx) != 0 {
			t.Error("last prompt tokens should be cleared by compaction")
		}
		if err := s.Reset(ctx); err != nil {
			t.Fatal(err)
		}
		left, _ = s.Messages(ctx)
		if sum, _ := s.Summary(ctx); len(left) != 0 || sum != "" {
			t.Errorf("reset failed: %v %q", left, sum)
		}
	})
}

func TestSanitizeHistory(t *testing.T) {
	mk := func(msgs ...llm.Message) []Stored {
		out := make([]Stored, len(msgs))
		for i, m := range msgs {
			out[i] = Stored{ID: int64(i + 1), Message: m}
		}
		return out
	}
	roles := func(in []Stored) string {
		var sb strings.Builder
		for _, s := range in {
			sb.WriteString(string(s.Message.Role)[:1])
		}
		return sb.String()
	}
	for name, tc := range map[string]struct {
		in   []Stored
		want string
	}{
		"clean":                   {mk(user("q"), call("1"), result("1"), assistant("a")), "uata"},
		"leading orphans dropped": {mk(result("0"), assistant("x"), user("q"), assistant("a")), "ua"},
		"call without result":     {mk(user("q"), call("1")), "u"},
		"partial results":         {mk(user("q"), llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{ID: "1"}, {ID: "2"}}}, result("1"), user("q2")), "uu"},
		"orphan tool mid":         {mk(user("q"), result("9"), assistant("a")), "ua"},
	} {
		if got := roles(sanitizeHistory(tc.in)); got != tc.want {
			t.Errorf("%s: got %q want %q", name, got, tc.want)
		}
	}
}

func TestSessionDirIsSafe(t *testing.T) {
	base := t.TempDir()
	for _, id := range []string{"../../etc/passwd", "-100123456", "a/b\\c", "", "..", strings.Repeat("x", 500), "ชื่อไทย"} {
		d := sessionDir(base, "tele/gram", id)
		rel, err := filepath.Rel(base, d)
		if err != nil || strings.HasPrefix(rel, "..") || strings.Count(rel, string(filepath.Separator)) != 1 {
			t.Errorf("id %q → %q escapes or is malformed (rel %q)", id, d, rel)
		}
	}
	if sessionDir(base, "t", "1") == sessionDir(base, "t", "2") {
		t.Error("distinct ids must map to distinct dirs")
	}
	// ids that sanitize identically must still differ
	if sessionDir(base, "t", "a/b") == sessionDir(base, "t", "a\\b") {
		t.Error("hash suffix must disambiguate")
	}
}

func TestPersistsAcrossManagers(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, 4, 4)
	_ = m.With(ctx, "c", "1", func(s *Session) error { return s.Append(ctx, user("remember me")) })
	m.Close()

	m = NewManager(dir, 4, 4)
	defer m.Close()
	_ = m.With(ctx, "c", "1", func(s *Session) error {
		msgs, _ := s.Messages(ctx)
		if len(msgs) != 1 || msgs[0].Message.Content != "remember me" {
			t.Errorf("msgs = %+v", msgs)
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(dir)); err != nil {
		t.Fatal(err)
	}
}

func TestSessionsAreIsolated(t *testing.T) {
	m := NewManager(t.TempDir(), 4, 4)
	defer m.Close()
	_ = m.With(ctx, "c", "1", func(s *Session) error { return s.Append(ctx, user("one")) })
	_ = m.With(ctx, "c", "2", func(s *Session) error {
		if msgs, _ := s.Messages(ctx); len(msgs) != 0 {
			t.Errorf("chat 2 sees chat 1 messages: %+v", msgs)
		}
		return nil
	})
}

func TestWithSerialisesPerChat(t *testing.T) {
	m := NewManager(t.TempDir(), 4, 100)
	defer m.Close()
	var running, maxRunning atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = m.With(ctx, "c", "1", func(*Session) error {
				n := running.Add(1)
				for {
					old := maxRunning.Load()
					if n <= old || maxRunning.CompareAndSwap(old, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				running.Add(-1)
				return nil
			})
		}()
	}
	wg.Wait()
	if maxRunning.Load() != 1 {
		t.Errorf("max concurrent = %d, want 1", maxRunning.Load())
	}
}

func TestBusyWhenQueueFull(t *testing.T) {
	m := NewManager(t.TempDir(), 4, 2)
	defer m.Close()
	pending := func() int {
		m.mu.Lock()
		defer m.mu.Unlock()
		if e := m.entries["c\x001"]; e != nil {
			return e.refs
		}
		return 0
	}
	started, release := make(chan struct{}), make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // running
		defer wg.Done()
		_ = m.With(ctx, "c", "1", func(*Session) error { close(started); <-release; return nil })
	}()
	<-started
	go func() { // queued behind the running request
		defer wg.Done()
		_ = m.With(ctx, "c", "1", func(*Session) error { return nil })
	}()
	deadline := time.Now().Add(2 * time.Second)
	for pending() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("queued request never registered")
		}
		time.Sleep(time.Millisecond)
	}
	if err := m.With(ctx, "c", "1", func(*Session) error { return nil }); !errors.Is(err, ErrBusy) {
		t.Errorf("err = %v, want ErrBusy", err)
	}
	close(release)
	wg.Wait()
}

func TestWithRespectsContext(t *testing.T) {
	m := NewManager(t.TempDir(), 4, 4)
	defer m.Close()
	started, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = m.With(ctx, "c", "1", func(*Session) error { close(started); <-release; return nil })
	}()
	<-started
	cctx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := m.With(cctx, "c", "1", func(*Session) error { return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v", err)
	}
	close(release)
}

func TestEvictionKeepsDataAndBoundsOpen(t *testing.T) {
	m := NewManager(t.TempDir(), 2, 4)
	defer m.Close()
	for i := 0; i < 6; i++ {
		id := string(rune('a' + i))
		if err := m.With(ctx, "c", id, func(s *Session) error { return s.Append(ctx, user("hi "+id)) }); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	open := len(m.entries)
	m.mu.Unlock()
	if open > 2 {
		t.Errorf("open entries = %d, want <= 2", open)
	}
	_ = m.With(ctx, "c", "a", func(s *Session) error { // reopened from disk
		if msgs, _ := s.Messages(ctx); len(msgs) != 1 || msgs[0].Message.Content != "hi a" {
			t.Errorf("evicted session lost data: %+v", msgs)
		}
		return nil
	})
}

func TestClosedManager(t *testing.T) {
	m := NewManager(t.TempDir(), 2, 2)
	m.Close()
	if err := m.With(ctx, "c", "1", func(*Session) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v", err)
	}
}

func TestRecentSkipsToolPlumbing(t *testing.T) {
	withSession(t, func(s *Session) {
		_ = s.Append(ctx, user("q1"), call("c1"), result("c1"), assistant("a1"), user("q2"), assistant("a2"))
		got, err := s.Recent(ctx, 10)
		if err != nil {
			t.Fatal(err)
		}
		want := []Turn{{"user", "q1"}, {"assistant", "a1"}, {"user", "q2"}, {"assistant", "a2"}}
		if len(got) != len(want) {
			t.Fatalf("got %+v", got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("turn %d = %+v, want %+v", i, got[i], want[i])
			}
		}
		last, _ := s.Recent(ctx, 2)
		if len(last) != 2 || last[0].Text != "q2" || last[1].Text != "a2" {
			t.Errorf("last two = %+v", last)
		}
		if none, _ := s.Recent(ctx, 0); len(none) != 0 {
			t.Errorf("n=0 returned %+v", none)
		}
	})
}

func TestPeekDoesNotWaitForARunningRequest(t *testing.T) {
	m := NewManager(t.TempDir(), 4, 1) // queue limit 1: With would answer ErrBusy
	defer m.Close()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.With(ctx, "c", "1", func(s *Session) error {
			_ = s.Append(ctx, user("in flight"))
			close(started)
			<-release
			return nil
		})
	}()
	<-started

	got := make(chan []Turn, 1)
	go func() {
		_ = m.Peek("c", "1", func(s *Session) error {
			turns, _ := s.Recent(ctx, 5)
			got <- turns
			return nil
		})
	}()
	select {
	case turns := <-got:
		if len(turns) != 1 || turns[0].Text != "in flight" {
			t.Errorf("turns = %+v", turns)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Peek blocked behind the running request")
	}
	close(release)
	<-done
	if err := m.With(ctx, "c", "1", func(*Session) error { return nil }); err != nil {
		t.Errorf("Peek left the queue in a bad state: %v", err)
	}
}

func TestPeekOnClosedManagerAndNewChat(t *testing.T) {
	m := NewManager(t.TempDir(), 4, 4)
	if err := m.Peek("c", "new", func(s *Session) error {
		if turns, _ := s.Recent(ctx, 5); len(turns) != 0 {
			t.Errorf("a new chat has history: %+v", turns)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if err := m.Peek("c", "new", func(*Session) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v", err)
	}
}
