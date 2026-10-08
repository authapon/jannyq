package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
		want := []Turn{{Role: "user", Text: "q1"}, {Role: "assistant", Text: "a1"}, {Role: "user", Text: "q2"}, {Role: "assistant", Text: "a2"}}
		if len(got) != len(want) {
			t.Fatalf("got %+v", got)
		}
		for i := range want {
			if got[i].Role != want[i].Role || got[i].Text != want[i].Text || len(got[i].Attachments) != 0 {
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
	peeked := make(chan struct{})
	go func() {
		defer close(peeked)
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
	<-peeked // Peek has returned and released its place in the queue
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

func TestAppendEntriesKeepsSenderAndTime(t *testing.T) {
	withSession(t, func(s *Session) {
		err := s.AppendEntries(ctx,
			Entry{Message: user("hello"), SenderID: "42", SenderName: "Ann", SentAt: "1997-07-16T19:20:44+01:00"},
			Entry{Message: assistant("hi Ann")})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := s.Messages(ctx)
		if len(got) != 2 {
			t.Fatalf("got %d messages", len(got))
		}
		u := got[0]
		if u.SenderID != "42" || u.SenderName != "Ann" || u.SentAt != "1997-07-16T19:20:44+01:00" || u.CreatedAt == 0 {
			t.Errorf("user row = %+v", u)
		}
		if a := got[1]; a.SenderID != "" || a.SentAt != "" {
			t.Errorf("assistant row must carry no sender: %+v", a)
		}
	})
}

// legacyDB creates a session database the way the first versions did, before
// sender and time were recorded.
func legacyDB(t *testing.T, base string) {
	t.Helper()
	dir := sessionDir(base, "tg", "group1")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "session.db")))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE messages (id INTEGER PRIMARY KEY AUTOINCREMENT, role TEXT NOT NULL, content TEXT NOT NULL DEFAULT '',
			tool_calls TEXT, tool_call_id TEXT NOT NULL DEFAULT '', tool_name TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL)`,
		`INSERT INTO meta(key, value) VALUES ('chat', 'tg:group1')`,
		`INSERT INTO messages(role, content, created_at) VALUES ('user', 'Ann: old question', 868990844)`,
		`INSERT INTO messages(role, content, created_at) VALUES ('assistant', 'old answer', 868990850)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
}

func TestOldDatabasesAreMigratedAndKeepTheirMessages(t *testing.T) {
	base := t.TempDir()
	legacyDB(t, base)
	m := NewManager(base, 4, 4)
	defer m.Close()
	err := m.With(ctx, "tg", "group1", func(s *Session) error {
		got, err := s.Messages(ctx)
		if err != nil || len(got) != 2 || got[0].Message.Content != "Ann: old question" {
			t.Fatalf("legacy messages = %+v err = %v", got, err)
		}
		if got[0].SentAt != "" || got[0].SenderName != "" || got[0].CreatedAt != 868990844 {
			t.Errorf("legacy row = %+v", got[0])
		}
		// the migrated database accepts new-style rows next to the old ones
		if err := s.AppendEntries(ctx, Entry{Message: user("new"), SenderID: "1", SenderName: "Bob", SentAt: "2026-10-06T14:32:05+07:00"}); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Messages(ctx)
		if len(got) != 3 || got[2].SenderName != "Bob" {
			t.Errorf("after append: %+v", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	m.Close()

	// reopening a migrated database is a no-op
	m = NewManager(base, 4, 4)
	defer m.Close()
	_ = m.With(ctx, "tg", "group1", func(s *Session) error {
		var v int
		if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil || v != schemaVersion {
			t.Errorf("user_version = %d err = %v", v, err)
		}
		if got, _ := s.Messages(ctx); len(got) != 3 {
			t.Errorf("messages after reopening: %d", len(got))
		}
		return nil
	})
}

func TestGroupFlag(t *testing.T) {
	withSession(t, func(s *Session) {
		if s.IsGroup(ctx) {
			t.Error("a new chat is not a group")
		}
		_ = s.SetGroup(ctx, true)
		if !s.IsGroup(ctx) {
			t.Error("flag not stored")
		}
		_ = s.Reset(ctx)
		if !s.IsGroup(ctx) {
			t.Error("/reset must not turn a group into a private chat")
		}
		_ = s.SetGroup(ctx, false)
		if s.IsGroup(ctx) {
			t.Error("flag not cleared")
		}
	})
}

func TestPruneKeepsTheNewestMessages(t *testing.T) {
	withSession(t, func(s *Session) {
		for i := 0; i < 10; i++ {
			_ = s.Append(ctx, user("q"+strconv.Itoa(i)))
		}
		_ = s.Append(ctx, call("c1"), result("c1"), assistant("a"))
		n, err := s.Prune(ctx, 4)
		if err != nil || n == 0 {
			t.Fatalf("pruned %d, err %v", n, err)
		}
		left, _ := s.Messages(ctx)
		var texts []string
		for _, m := range left {
			if m.Message.Role == llm.RoleUser {
				texts = append(texts, m.Message.Content)
			}
		}
		if got := strings.Join(texts, ","); got != "q7,q8,q9" {
			t.Errorf("remaining user messages = %s", got)
		}
		if c, _ := s.Count(ctx); c != 4 {
			t.Errorf("count = %d, want 4", c)
		}
		// nothing to prune
		if n, err := s.Prune(ctx, 100); err != nil || n != 0 {
			t.Errorf("pruned %d, err %v", n, err)
		}
	})
	withSession(t, func(s *Session) {
		if n, err := s.Prune(ctx, 3); err != nil || n != 0 {
			t.Errorf("empty chat: %d %v", n, err)
		}
	})
}

func TestRecordWhileARequestIsRunning(t *testing.T) {
	m := NewManager(t.TempDir(), 4, 1) // a queued request would be refused
	defer m.Close()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.With(ctx, "c", "1", func(s *Session) error {
			_ = s.Append(ctx, user("question"))
			close(started)
			<-release
			return s.Append(ctx, assistant("answer"))
		})
	}()
	<-started
	for i := 0; i < 20; i++ {
		err := m.Record("c", "1", func(s *Session) error {
			return s.AppendEntries(ctx, Entry{Message: user("chatter " + strconv.Itoa(i)), SenderID: "9", SenderName: "Bob", SentAt: "2026-10-06T14:00:00+07:00"})
		})
		if err != nil {
			t.Fatalf("recording %d failed: %v", i, err)
		}
	}
	close(release)
	<-done
	_ = m.With(ctx, "c", "1", func(s *Session) error {
		got, _ := s.Messages(ctx)
		if len(got) != 22 || got[0].Message.Content != "question" || got[21].Message.Content != "answer" {
			t.Errorf("history has %d messages", len(got))
		}
		return nil
	})
}

func TestAppendEntryReturnsIdsAndLastID(t *testing.T) {
	withSession(t, func(s *Session) {
		if id, err := s.LastID(ctx); err != nil || id != 0 {
			t.Errorf("empty chat: id=%d err=%v", id, err)
		}
		a, err := s.AppendEntry(ctx, Entry{Message: user("one"), SenderID: "1", SenderName: "Ann", SentAt: "2026-10-06T10:00:00+07:00"})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := s.AppendEntry(ctx, Entry{Message: user("two")})
		if a <= 0 || b <= a {
			t.Errorf("ids = %d, %d", a, b)
		}
		if last, _ := s.LastID(ctx); last != b {
			t.Errorf("LastID = %d, want %d", last, b)
		}
		got, _ := s.Messages(ctx)
		if got[0].ID != a || got[0].SenderName != "Ann" || got[1].ID != b {
			t.Errorf("stored = %+v", got)
		}
	})
}

func TestResetUpToKeepsLaterMessages(t *testing.T) {
	withSession(t, func(s *Session) {
		_ = s.Append(ctx, user("old 1"), assistant("old 2"))
		mark, _ := s.LastID(ctx)
		_ = s.Append(ctx, user("after 1"), assistant("after 2"))
		_ = s.SetMeta(ctx, metaSummary, "summary of the old")
		if err := s.ResetUpTo(ctx, mark); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Messages(ctx)
		if len(got) != 2 || got[0].Message.Content != "after 1" {
			t.Errorf("after the reset: %+v", got)
		}
		if sum, _ := s.Summary(ctx); sum != "" {
			t.Errorf("the summary of forgotten messages survived: %q", sum)
		}
		// resetting up to 0 forgets nothing but the summary
		_ = s.SetMeta(ctx, metaSummary, "x")
		_ = s.ResetUpTo(ctx, 0)
		if n, _ := s.Count(ctx); n != 2 {
			t.Errorf("count = %d", n)
		}
	})
}

func TestAttachmentsRoundTripAndCleanup(t *testing.T) {
	withSession(t, func(s *Session) {
		dir, err := s.FilesDir()
		if err != nil {
			t.Fatal(err)
		}
		mkfile := func(name, content string) string {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			return filepath.Join("files", name)
		}
		m1, _ := s.AppendEntry(ctx, Entry{Message: user("look at this")})
		m2, _ := s.AppendEntry(ctx, Entry{Message: user("and this")})
		a1, err := s.AddAttachment(ctx, Attachment{MessageID: m1, Kind: "image", Name: "cat.jpg", MIME: "image/jpeg", Size: 5000,
			Path: mkfile("1.jpg", "jpg"), Images: []string{mkfile("1.jpg", "jpg")}, StoredBytes: 3})
		if err != nil {
			t.Fatal(err)
		}
		a2, _ := s.AddAttachment(ctx, Attachment{MessageID: m2, Kind: "pdf", Name: "doc.pdf", MIME: "application/pdf", Size: 9000, Pages: 12, Chars: 3400,
			Path: mkfile("2.pdf", "pdf"), TextPath: mkfile("2.txt", "text"), Inline: true, Note: "ok", StoredBytes: 7})

		got, err := s.AttachmentByID(ctx, a2)
		if err != nil || got.Name != "doc.pdf" || got.Pages != 12 || !got.Inline || got.TextPath != "files/2.txt" || got.MessageID != m2 {
			t.Fatalf("attachment = %+v err = %v", got, err)
		}
		if _, err := s.AttachmentByID(ctx, 999); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("unknown id: %v", err)
		}
		all, _ := s.AttachmentsFrom(ctx, m1)
		if len(all[m1]) != 1 || all[m1][0].ID != a1 || len(all[m1][0].Images) != 1 || len(all[m2]) != 1 {
			t.Errorf("grouped = %+v", all)
		}
		if later, _ := s.AttachmentsFrom(ctx, m2); len(later) != 1 || len(later[m2]) != 1 {
			t.Errorf("from m2 = %+v", later)
		}
		turns, _ := s.Recent(ctx, 10)
		if len(turns[0].Attachments) != 1 || turns[0].Attachments[0] != "cat.jpg" || turns[1].Attachments[0] != "doc.pdf" {
			t.Errorf("web history = %+v", turns)
		}

		// forgetting the first message deletes its attachment and its files, not the other's
		if err := s.ResetUpTo(ctx, m1); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AttachmentByID(ctx, a1); err == nil {
			t.Error("the attachment of a forgotten message survived")
		}
		if _, err := os.Stat(filepath.Join(dir, "1.jpg")); !os.IsNotExist(err) {
			t.Errorf("its file survived: %v", err)
		}
		if _, err := s.AttachmentByID(ctx, a2); err != nil {
			t.Errorf("the other attachment was deleted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dir, "2.pdf")); err != nil {
			t.Errorf("its file was deleted: %v", err)
		}
		if err := s.Reset(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "2.txt")); !os.IsNotExist(err) {
			t.Errorf("a full reset left files behind: %v", err)
		}
	})
}

func TestAttachmentPathsCannotEscapeTheSession(t *testing.T) {
	withSession(t, func(s *Session) {
		outside := filepath.Join(filepath.Dir(s.Dir()), "precious.txt")
		if err := os.WriteFile(outside, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		m, _ := s.AppendEntry(ctx, Entry{Message: user("x")})
		_, _ = s.AddAttachment(ctx, Attachment{MessageID: m, Kind: "pdf", Name: "evil", Path: "../precious.txt", TextPath: outside, Images: []string{"../../etc/hostname"}})
		if err := s.Reset(ctx); err != nil {
			t.Fatal(err)
		}
		if b, err := os.ReadFile(outside); err != nil || string(b) != "keep me" {
			t.Errorf("a stored path made the session delete a file outside it: %q %v", b, err)
		}
		for _, bad := range []string{"", "/etc/passwd", "../x", "files/../../x", ".."} {
			if _, err := s.FilePath(bad); err == nil {
				t.Errorf("FilePath(%q) accepted", bad)
			}
		}
		if p, err := s.FilePath("files/ok.txt"); err != nil || !strings.HasPrefix(p, s.Dir()) {
			t.Errorf("FilePath = %q %v", p, err)
		}
	})
}

func TestEvictAttachmentsKeepsTheNewestAndTheLiveOnes(t *testing.T) {
	withSession(t, func(s *Session) {
		dir, _ := s.FilesDir()
		add := func(msg int64, name string, size int64) int64 {
			rel := filepath.Join("files", name)
			_ = os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o600)
			id, _ := s.AddAttachment(ctx, Attachment{MessageID: msg, Kind: "pdf", Name: name, Path: rel, StoredBytes: size})
			return id
		}
		old, _ := s.AppendEntry(ctx, Entry{Message: user("old")})
		add(old, "a.pdf", 400)
		mid, _ := s.AppendEntry(ctx, Entry{Message: user("mid")})
		b := add(mid, "b.pdf", 400)
		last, _ := s.AppendEntry(ctx, Entry{Message: user("last")})
		c := add(last, "c.pdf", 400)
		// the message of the oldest attachment was compacted away
		_ = s.Compact(ctx, old, "summary")

		if n, err := s.EvictAttachments(ctx, 2000); err != nil || n != 0 {
			t.Errorf("under the limit: removed %d err %v", n, err)
		}
		n, err := s.EvictAttachments(ctx, 900) // 1200 bytes stored: one must go, the orphan first
		if err != nil || n != 1 {
			t.Fatalf("removed %d, err %v", n, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "a.pdf")); !os.IsNotExist(err) {
			t.Error("the attachment of the compacted message should have gone first")
		}
		if _, err := s.AttachmentByID(ctx, b); err != nil {
			t.Error("a live attachment was evicted before the orphan")
		}
		n, _ = s.EvictAttachments(ctx, 500) // now the older live one
		if n != 1 {
			t.Errorf("removed %d", n)
		}
		if _, err := s.AttachmentByID(ctx, c); err != nil {
			t.Error("the newest attachment must be the last to go")
		}
	})
}

func TestVersion1DatabasesGainTheAttachmentsTable(t *testing.T) {
	base := t.TempDir()
	legacyDB(t, base) // version 0
	m := NewManager(base, 4, 4)
	_ = m.With(ctx, "tg", "group1", func(s *Session) error { return nil }) // migrates to the current version
	m.Close()
	// now pretend it had stopped at version 1: no attachments table
	dir := sessionDir(base, "tg", "group1")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(filepath.Join(dir, "session.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DROP TABLE attachments`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	m = NewManager(base, 4, 4)
	defer m.Close()
	err = m.With(ctx, "tg", "group1", func(s *Session) error {
		mid, _ := s.AppendEntry(ctx, Entry{Message: user("new")})
		if _, err := s.AddAttachment(ctx, Attachment{MessageID: mid, Kind: "text", Name: "n.txt"}); err != nil {
			t.Errorf("AddAttachment after the upgrade: %v", err)
		}
		if got, _ := s.Messages(ctx); len(got) != 3 {
			t.Errorf("old messages lost: %d", len(got))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSweepDeletesIdleChatsOnly(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, 8, 4)
	defer m.Close()
	ctx := context.Background()
	write := func(channel, chat string) {
		if err := m.With(ctx, channel, chat, func(s *Session) error {
			if _, err := s.FilesDir(); err != nil {
				return err
			}
			return s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "hi"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("telegram", "old")
	write("telegram", "fresh")
	write("web", "old2")
	if m.OpenCount() != 3 {
		t.Errorf("open = %d", m.OpenCount())
	}
	// "old" and "old2" were last written 40 days ago
	past := time.Now().Add(-40 * 24 * time.Hour)
	for _, rel := range []string{"telegram", "web"} {
		entries, _ := os.ReadDir(filepath.Join(dir, rel))
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "old") {
				for _, f := range []string{"session.db", "session.db-wal"} {
					_ = os.Chtimes(filepath.Join(dir, rel, e.Name(), f), past, past)
				}
			}
		}
	}
	// a directory that is not a chat is left alone
	if err := os.MkdirAll(filepath.Join(dir, "telegram", "not-a-chat"), 0o755); err != nil {
		t.Fatal(err)
	}
	n, err := m.Sweep(30*24*time.Hour, time.Now())
	if err != nil || n != 2 {
		t.Fatalf("deleted %d: %v", n, err)
	}
	if m.Exists("telegram", "old") || m.Exists("web", "old2") || !m.Exists("telegram", "fresh") {
		t.Error("the wrong chats were deleted")
	}
	if _, err := os.Stat(filepath.Join(dir, "telegram", "not-a-chat")); err != nil {
		t.Error("a directory that is not a chat was deleted")
	}
	if m.OpenCount() != 1 {
		t.Errorf("open = %d: databases of deleted chats must be closed", m.OpenCount())
	}
	// the chat starts afresh when its person comes back
	write("telegram", "old")
	var count int
	_ = m.With(ctx, "telegram", "old", func(s *Session) error { count, _ = s.Count(ctx); return nil })
	if count != 1 {
		t.Errorf("a deleted chat kept %d messages", count)
	}
}

func TestSweepSparesChatsInUse(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir, 8, 4)
	defer m.Close()
	ctx := context.Background()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		_ = m.With(ctx, "telegram", "busy", func(s *Session) error {
			_ = s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "working"})
			past := time.Now().Add(-99 * 24 * time.Hour)
			_ = os.Chtimes(filepath.Join(s.Dir(), "session.db"), past, past)
			_ = os.Chtimes(filepath.Join(s.Dir(), "session.db-wal"), past, past)
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	n, err := m.Sweep(time.Hour, time.Now())
	close(release)
	<-done
	if err != nil || n != 0 || !m.Exists("telegram", "busy") {
		t.Errorf("a chat that is being served was deleted: %d %v", n, err)
	}
}

func TestSweepOfAMissingDirectory(t *testing.T) {
	m := NewManager(filepath.Join(t.TempDir(), "nope"), 8, 4)
	if n, err := m.Sweep(time.Hour, time.Now()); n != 0 || err != nil {
		t.Errorf("%d %v", n, err)
	}
}

func TestFirstReply(t *testing.T) {
	withSession(t, func(s *Session) {
		// questions alone do not count as having been answered
		if err := s.Append(ctx, user("hello")); err != nil {
			t.Fatal(err)
		}
		if first, err := s.FirstReply(ctx); err != nil || !first {
			t.Fatalf("first = %v, err = %v", first, err)
		}
		if first, _ := s.FirstReply(ctx); first {
			t.Error("a chat is greeted once")
		}
		if err := s.Reset(ctx); err != nil {
			t.Fatal(err)
		}
		if first, _ := s.FirstReply(ctx); first {
			t.Error("a reset must not bring the greeting back")
		}
		// a greeting that failed is offered again, once
		if err := s.RetryFirstReply(ctx); err != nil {
			t.Fatal(err)
		}
		if first, _ := s.FirstReply(ctx); !first {
			t.Error("a failed greeting should be retried")
		}
		if first, _ := s.FirstReply(ctx); first {
			t.Error("the retry happens once")
		}
	})

	// a chat that already has answers (it predates the greeting) is marked, not greeted
	withSession(t, func(s *Session) {
		if err := s.Append(ctx, user("hello"), assistant("hi")); err != nil {
			t.Fatal(err)
		}
		if first, err := s.FirstReply(ctx); err != nil || first {
			t.Errorf("existing chat: first = %v, err = %v", first, err)
		}
	})
}
