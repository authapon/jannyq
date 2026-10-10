package trigger

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

var bg = context.Background()

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "triggers.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func sample(next time.Time) Trigger {
	return Trigger{Channel: "telegram", ChatID: "42", OwnerID: "7", OwnerName: "Ann", Mode: ModeRemind, Title: "call Peter",
		Text: "Call Peter about the quote", At: next, Zone: "Asia/Bangkok", Next: next}
}

func TestStoreRoundTripAndChatIsolation(t *testing.T) {
	s, _ := openTest(t)
	when := time.Unix(1_800_000_000, 0)
	id, err := s.Create(bg, sample(when))
	if err != nil || id == 0 {
		t.Fatalf("%d %v", id, err)
	}
	got, err := s.Get(bg, "telegram", "42", id)
	if err != nil || got.Text != "Call Peter about the quote" || !got.Next.Equal(when) || got.Status != StatusActive ||
		got.OwnerName != "Ann" || got.Recurring() || got.Zone != "Asia/Bangkok" || got.Created.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	// another chat can neither see, change nor delete it
	if _, err := s.Get(bg, "telegram", "43", id); err != ErrNotFound {
		t.Errorf("get from another chat: %v", err)
	}
	if err := s.Delete(bg, "telegram", "43", id); err != ErrNotFound {
		t.Errorf("delete from another chat: %v", err)
	}
	other := got
	other.ChatID = "43"
	if err := s.Update(bg, other); err != ErrNotFound {
		t.Errorf("update from another chat: %v", err)
	}
	if l, _ := s.List(bg, "telegram", "43"); len(l) != 0 {
		t.Error("list leaks")
	}
	got.Text, got.Mode = "changed", ModeTask
	if err := s.Update(bg, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Get(bg, "telegram", "42", id); again.Text != "changed" || again.Mode != ModeTask {
		t.Errorf("%+v", again)
	}
	if n, _ := s.Count(bg, "telegram", "42"); n != 1 {
		t.Errorf("count = %d", n)
	}
	if err := s.Delete(bg, "telegram", "42", id); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.Count(bg, "telegram", "42"); n != 0 {
		t.Errorf("count after delete = %d", n)
	}
}

func TestTriggersSurviveARestart(t *testing.T) {
	s, path := openTest(t)
	when := time.Unix(1_800_000_000, 0)
	once, _ := s.Create(bg, sample(when))
	c := sample(when)
	c.Cron, c.At = "0 7 * * *", time.Time{}
	rec, _ := s.Create(bg, c)
	// a one-time trigger that was being run when the process stopped
	if ok, err := s.Claim(bg, Trigger{ID: once, Next: when}, time.Time{}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if got, _ := s.Get(bg, "telegram", "42", once); got.Status != StatusRunning {
		t.Fatalf("status = %q", got.Status)
	}
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	l, _ := s2.List(bg, "telegram", "42")
	if len(l) != 2 || l[0].ID != once || l[1].ID != rec {
		t.Fatalf("%+v", l)
	}
	if l[0].Status != StatusActive || !l[0].Next.Equal(when) {
		t.Errorf("an interrupted one-time trigger must be due again: %+v", l[0])
	}
	if l[1].Cron != "0 7 * * *" || !l[1].Recurring() {
		t.Errorf("%+v", l[1])
	}
}

func TestDueClaimAndFinish(t *testing.T) {
	s, _ := openTest(t)
	now := time.Unix(1_800_000_000, 0)
	past, _ := s.Create(bg, sample(now.Add(-time.Minute)))
	future, _ := s.Create(bg, sample(now.Add(time.Hour)))
	due, _ := s.Due(bg, now, 10)
	if len(due) != 1 || due[0].ID != past {
		t.Fatalf("due = %+v", due)
	}
	if next, ok, _ := s.NextDue(bg); !ok || !next.Equal(now.Add(-time.Minute)) {
		t.Errorf("next = %v", next)
	}
	// a claim works once
	if ok, _ := s.Claim(bg, due[0], time.Time{}); !ok {
		t.Fatal("first claim")
	}
	if ok, _ := s.Claim(bg, due[0], time.Time{}); ok {
		t.Error("second claim of the same trigger")
	}
	if d, _ := s.Due(bg, now, 10); len(d) != 0 {
		t.Errorf("a claimed trigger is not due: %+v", d)
	}
	s.now = func() time.Time { return now }
	if err := s.Finish(bg, due[0], "ok", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(bg, "telegram", "42", past)
	if got.Status != StatusDone || got.Runs != 1 || got.LastResult != "ok" || got.Next != (time.Time{}) {
		t.Errorf("%+v", got)
	}
	// a finished one-time trigger is dropped after a week
	if n, _ := s.Prune(bg, now.Add(-time.Hour)); n != 0 {
		t.Errorf("pruned too early: %d", n)
	}
	if n, _ := s.Prune(bg, now.Add(time.Hour)); n != 1 {
		t.Errorf("pruned %d", n)
	}
	if _, err := s.Get(bg, "telegram", "42", future); err != nil {
		t.Errorf("an unfinished trigger must stay: %v", err)
	}

	// a recurring trigger moves on to its next time
	c := sample(now.Add(-time.Minute))
	c.Cron = "*/30 * * * *"
	cid, _ := s.Create(bg, c)
	d, _ := s.Due(bg, now, 10)
	if len(d) != 1 || d[0].ID != cid {
		t.Fatalf("%+v", d)
	}
	nxt := now.Add(30 * time.Minute)
	if ok, _ := s.Claim(bg, d[0], nxt); !ok {
		t.Fatal("claim")
	}
	_ = s.Finish(bg, d[0], "failed", "boom")
	got, _ = s.Get(bg, "telegram", "42", cid)
	if got.Status != StatusActive || !got.Next.Equal(nxt) || got.Fails != 1 || got.LastError != "boom" || got.Runs != 1 {
		t.Errorf("%+v", got)
	}
}

func TestDisableOrphansAndStats(t *testing.T) {
	s, _ := openTest(t)
	now := time.Unix(1_800_000_000, 0)
	a, _ := s.Create(bg, sample(now.Add(time.Hour)))
	task := sample(now.Add(time.Hour))
	task.Mode, task.ChatID = ModeTask, "99"
	_, _ = s.Create(bg, task)
	if err := s.Disable(bg, a, "the owner may not use the bot"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(bg, "telegram", "42", a)
	if got.Status != StatusDisabled || got.LastError == "" || !got.Next.IsZero() {
		t.Errorf("%+v", got)
	}
	if d, _ := s.Due(bg, now.Add(24*time.Hour), 10); len(d) != 1 || d[0].Mode != ModeTask {
		t.Errorf("a disabled trigger must not be due: %+v", d)
	}
	r, tk, _ := s.Stats(bg)
	if r != 0 || tk != 1 {
		t.Errorf("stats = %d %d", r, tk)
	}
	// triggers of a chat that no longer exists go with it
	n, err := s.DeleteOrphans(bg, func(channel, chat string) bool { return chat == "99" })
	if err != nil || n != 1 {
		t.Errorf("orphans deleted: %d %v", n, err)
	}
	if l, _ := s.List(bg, "telegram", "42"); len(l) != 0 {
		t.Error("the orphan is still there")
	}
	if l, _ := s.List(bg, "telegram", "99"); len(l) != 1 {
		t.Error("the other chat lost its trigger")
	}
}

func TestNotifyFieldsRoundTripAndPrefs(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	tr := sample(time.Now().Add(time.Hour))
	tr.Notify, tr.NtfyTopic, tr.Priority = NotifyBoth, "work", 4
	id, err := s.Create(ctx, tr)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, tr.Channel, tr.ChatID, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Notify != NotifyBoth || got.NtfyTopic != "work" || got.Priority != 4 {
		t.Fatalf("round trip: %+v", got)
	}
	got.Notify, got.NtfyTopic, got.Priority = NotifyNtfy, "", 0
	if err := s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Get(ctx, tr.Channel, tr.ChatID, id)
	if got.Notify != NotifyNtfy || got.NtfyTopic != "" || got.Priority != 0 {
		t.Fatalf("after update: %+v", got)
	}
	// a trigger made without a way of delivery goes to the chat
	id2, _ := s.Create(ctx, sample(time.Now().Add(time.Hour)))
	if g, _ := s.Get(ctx, tr.Channel, tr.ChatID, id2); g.Notify != NotifyChat {
		t.Fatalf("default notify = %q", g.Notify)
	}

	p, err := s.GetPrefs(ctx, "telegram", "42")
	if err != nil || p.NtfyTopic != "" || p.DefaultNotify != "" {
		t.Fatalf("no prefs yet: %+v %v", p, err)
	}
	if err := s.SetPrefs(ctx, Prefs{Channel: "telegram", UserID: "42", NtfyTopic: "me", DefaultNotify: NotifyBoth}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPrefs(ctx, Prefs{Channel: "telegram", UserID: "43", NtfyTopic: "other"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetPrefs(ctx, "telegram", "42"); p.NtfyTopic != "me" || p.DefaultNotify != NotifyBoth {
		t.Fatalf("prefs: %+v", p)
	}
	if p, _ := s.GetPrefs(ctx, "discord", "42"); p.NtfyTopic != "" {
		t.Fatalf("prefs leaked across channels: %+v", p)
	}
	if err := s.SetPrefs(ctx, Prefs{Channel: "telegram", UserID: "42"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.GetPrefs(ctx, "telegram", "42"); p.NtfyTopic != "" || p.DefaultNotify != "" {
		t.Fatalf("prefs not cleared: %+v", p)
	}
	if p, _ := s.GetPrefs(ctx, "telegram", "43"); p.NtfyTopic != "other" {
		t.Fatalf("the other user's prefs were touched: %+v", p)
	}
}

func TestOpenUpgradesAnOldDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := `CREATE TABLE triggers (
	id INTEGER PRIMARY KEY AUTOINCREMENT, channel TEXT NOT NULL, chat_id TEXT NOT NULL, is_group INTEGER NOT NULL DEFAULT 0,
	owner_id TEXT NOT NULL, owner_name TEXT NOT NULL DEFAULT '', mode TEXT NOT NULL, title TEXT NOT NULL DEFAULT '', text TEXT NOT NULL,
	cron TEXT NOT NULL DEFAULT '', at_unix INTEGER NOT NULL DEFAULT 0, zone TEXT NOT NULL DEFAULT '', next_unix INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL, created INTEGER NOT NULL, last_run INTEGER NOT NULL DEFAULT 0, last_result TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '', runs INTEGER NOT NULL DEFAULT 0, fails INTEGER NOT NULL DEFAULT 0);
	INSERT INTO triggers(channel, chat_id, owner_id, mode, text, status, created) VALUES ('cli','1','u','remind','old one','active',1);`
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	list, err := s.List(context.Background(), "cli", "1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	if list[0].Notify != NotifyChat || list[0].NtfyTopic != "" || list[0].Priority != 0 {
		t.Fatalf("old trigger: %+v", list[0])
	}
	s.Close()
	if s, err = Open(path); err != nil { // opening again changes nothing
		t.Fatal(err)
	}
}
