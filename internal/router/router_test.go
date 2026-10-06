package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/agent"
	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/i18n"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/sandbox"
	"github.com/authapon/jannyq/internal/session"
)

var ctx = context.Background()

type fakeLLM struct {
	mu       sync.Mutex
	requests []llm.Request
	reply    func(llm.Request) (*llm.Response, error)
}

func (f *fakeLLM) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	if f.reply != nil {
		return f.reply(req)
	}
	return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "pong"}}, nil
}

func (f *fakeLLM) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type recorder struct {
	mu   sync.Mutex
	sent []string
}

func (r *recorder) Send(_ context.Context, text string) error {
	r.mu.Lock()
	r.sent = append(r.sent, text)
	r.mu.Unlock()
	return nil
}
func (r *recorder) Typing(context.Context) error { return nil }
func (r *recorder) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sent...)
}

func newRouter(t *testing.T, p llm.Provider, cfg Config) *Router {
	t.Helper()
	return newRouterWith(t, p, cfg, agent.Config{Model: "m", CompactAfter: 200, CompactKeep: 20})
}

func newRouterWith(t *testing.T, p llm.Provider, cfg Config, acfg agent.Config) *Router {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := session.NewManager(t.TempDir(), 8, 4)
	t.Cleanup(func() { sm.Close() })
	if acfg.Location == nil {
		acfg.Location = time.UTC
	}
	ag := agent.New(acfg, p, nil, log)
	return New(cfg, sm, ag, i18n.New("en"), log)
}

func msg(rec *recorder, text string) channel.Incoming {
	return channel.Incoming{Channel: "test", ChatID: "c1", UserID: "u1", UserName: "Ann", Text: text, Addressed: true, Responder: rec}
}

func TestPrivateChatReply(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	r.Handle(ctx, msg(rec, "ping"))
	if got := rec.all(); len(got) != 1 || got[0] != "pong" {
		t.Errorf("sent = %q", got)
	}
	// context is remembered across messages
	r.Handle(ctx, msg(rec, "again"))
	f.mu.Lock()
	second := f.requests[1].Messages
	f.mu.Unlock()
	if len(second) != 4 || !strings.HasSuffix(second[1].Content, "] ping") || !strings.HasPrefix(second[1].Content, "[") || second[2].Content != "pong" {
		t.Errorf("second request lacks history: %+v", second)
	}
}

func TestGroupPolicy(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	in := msg(rec, "chatter between humans")
	in.IsGroup, in.Addressed = true, false
	r.Handle(ctx, in)
	if f.calls() != 0 || len(rec.all()) != 0 {
		t.Error("unaddressed group message must be ignored")
	}
	in.Addressed = true
	r.Handle(ctx, in)
	if f.calls() != 1 {
		t.Error("addressed group message must be answered")
	}
	f.mu.Lock()
	// the unaddressed message was kept, and the model sees it, with who said it
	msgs := f.requests[0].Messages
	if len(msgs) != 3 || !strings.HasSuffix(msgs[1].Content, " Ann#"+tagOf(msgs[1].Content)+": chatter between humans") {
		t.Errorf("messages sent to the model: %+v", msgs)
	}
	f.mu.Unlock()

	r2 := newRouter(t, f, Config{GroupReply: GroupReplyAll})
	in.Addressed = false
	r2.Handle(ctx, in)
	if f.calls() != 2 {
		t.Error("group-reply=all must answer unaddressed messages")
	}
}

// tagOf extracts the tag from a header such as "[time] Ann#7f3a: text".
func tagOf(content string) string {
	i := strings.Index(content, "#")
	if i < 0 || len(content) < i+5 {
		return ""
	}
	return content[i+1 : i+5]
}

func TestAllowedUsers(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{AllowedUsers: []string{"u1", "other:7"}})
	rec := &recorder{}
	r.Handle(ctx, msg(rec, "hi"))
	if f.calls() != 1 {
		t.Error("allowed user rejected")
	}
	rec2 := &recorder{}
	in := msg(rec2, "hi")
	in.UserID = "u2"
	r.Handle(ctx, in)
	if f.calls() != 1 || len(rec2.all()) != 1 || !strings.Contains(rec2.all()[0], "not allowed") {
		t.Errorf("blocked user got %q", rec2.all())
	}
	r = newRouter(t, f, Config{AllowedUsers: []string{"TEST:u3"}})
	in.UserID = "u3"
	r.Handle(ctx, in)
	if f.calls() != 2 {
		t.Error("channel-qualified allow entry (case-insensitive) rejected")
	}
}

func TestRateLimit(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{RateLimit: 2})
	rec := &recorder{}
	for i := 0; i < 3; i++ {
		r.Handle(ctx, msg(rec, "hi"))
	}
	got := rec.all()
	if len(got) != 3 || got[0] != "pong" || got[1] != "pong" || !strings.Contains(got[2], "too quickly") {
		t.Errorf("sent = %q", got)
	}
	if f.calls() != 2 {
		t.Errorf("model calls = %d, want 2", f.calls())
	}
}

func TestCommands(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}

	r.Handle(ctx, msg(rec, "/help"))
	r.Handle(ctx, msg(rec, "/start"))
	if got := rec.all(); len(got) != 2 || !strings.Contains(got[0], "/reset") {
		t.Errorf("help = %q", got)
	}
	if f.calls() != 0 {
		t.Error("commands must not reach the model")
	}

	r.Handle(ctx, msg(rec, "remember: blue"))
	r.Handle(ctx, msg(rec, "/RESET"))
	r.Handle(ctx, msg(rec, "what colour?"))
	f.mu.Lock()
	last := f.requests[len(f.requests)-1].Messages
	f.mu.Unlock()
	if len(last) != 2 {
		t.Errorf("after /reset the model should see only the new message, got %d messages", len(last))
	}

	// unknown slash text is an ordinary message
	before := f.calls()
	r.Handle(ctx, msg(rec, "/etc/hosts what is it"))
	if f.calls() != before+1 {
		t.Error("unknown /text must go to the model")
	}

	r.Handle(ctx, msg(rec, "/compact"))
	if got := rec.all(); !strings.Contains(got[len(got)-1], "nothing to compact") {
		t.Errorf("compact on short chat = %q", got[len(got)-1])
	}
}

func TestAttachmentOnlyMessage(t *testing.T) {
	r := newRouter(t, &fakeLLM{}, Config{})
	rec := &recorder{}
	in := msg(rec, "")
	in.HasAttachment = true
	r.Handle(ctx, in)
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "only read pictures") {
		t.Errorf("sent = %q", got)
	}
	rec2 := &recorder{}
	r.Handle(ctx, msg(rec2, "   "))
	if len(rec2.all()) != 0 {
		t.Error("blank text without attachment must be ignored")
	}
}

func TestModelFailureAndEmptyReply(t *testing.T) {
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) { return nil, errors.New("down") }}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	r.Handle(ctx, msg(rec, "hi"))
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "something went wrong") {
		t.Errorf("sent = %q", got)
	}

	f2 := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "  "}}, nil
	}}
	r = newRouter(t, f2, Config{})
	rec = &recorder{}
	r.Handle(ctx, msg(rec, "hi"))
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "rephrase") {
		t.Errorf("sent = %q", got)
	}
}

func TestBusyWhenFlooded(t *testing.T) {
	gate := make(chan struct{})
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		<-gate
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "slow"}}, nil
	}}
	r := newRouter(t, f, Config{RateLimit: 0})
	rec := &recorder{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // fills the 4 slots: 1 running + 3 queued
		wg.Add(1)
		go func() { defer wg.Done(); r.Handle(ctx, msg(rec, "q")) }()
	}
	deadline := time.Now().Add(3 * time.Second)
	for f.calls() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the other three queue up
	rec2 := &recorder{}
	r.Handle(ctx, msg(rec2, "one too many"))
	if got := rec2.all(); len(got) != 1 || !strings.Contains(got[0], "still working") {
		t.Errorf("overflow message got %q", got)
	}
	close(gate)
	wg.Wait()
}

func TestDifferentChatsAreIndependent(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	r.Handle(ctx, msg(rec, "chat one"))
	in := msg(rec, "chat two")
	in.ChatID = "c2"
	r.Handle(ctx, in)
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.requests[1].Messages); n != 2 {
		t.Errorf("chat 2 request has %d messages, want 2 (no leakage from chat 1)", n)
	}
}

func TestResetAlsoDeletesTheSandboxWorkspace(t *testing.T) {
	r := newRouter(t, &fakeLLM{}, Config{})
	var deleted []string
	r.SetWorkspaceReset(func(_ context.Context, ws string) error { deleted = append(deleted, ws); return nil })
	rec := &recorder{}
	r.Handle(ctx, msg(rec, "hello"))
	r.Handle(ctx, msg(rec, "/reset"))
	if len(deleted) != 1 || deleted[0] != sandbox.WorkspaceID("test:c1") {
		t.Errorf("deleted = %v", deleted)
	}
	if got := rec.all(); !strings.Contains(got[len(got)-1], "cleared") {
		t.Errorf("reply = %q", got[len(got)-1])
	}

	// a failing workspace delete must not break /reset
	r.SetWorkspaceReset(func(context.Context, string) error { return errors.New("sandbox down") })
	r.Handle(ctx, msg(rec, "/reset"))
	if got := rec.all(); !strings.Contains(got[len(got)-1], "cleared") {
		t.Errorf("reply after failed workspace reset = %q", got[len(got)-1])
	}
}

func group(rec *recorder, user, name, text string, addressed bool) channel.Incoming {
	return channel.Incoming{Channel: "tg", ChatID: "g1", UserID: user, UserName: name, Text: text,
		IsGroup: true, Addressed: addressed, Responder: rec}
}

func TestGroupConversationIsKeptAndShownToTheModel(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	at := time.Date(2026, 10, 6, 7, 32, 5, 0, time.UTC)
	for i, m := range []struct{ user, name, text string }{
		{"1", "Ann", "shall we meet at noon?"}, {"2", "Bob", "noon works"}, {"1", "Ann", "great"},
	} {
		in := group(rec, m.user, m.name, m.text, false)
		in.ReceivedAt = at.Add(time.Duration(i) * time.Minute)
		r.Handle(ctx, in)
	}
	if f.calls() != 0 || len(rec.all()) != 0 {
		t.Fatalf("chatter must neither reach the model nor be answered (calls=%d sent=%q)", f.calls(), rec.all())
	}

	ask := group(rec, "2", "Bob", "@bot what did we agree?", true)
	ask.ReceivedAt = at.Add(10 * time.Minute)
	r.Handle(ctx, ask)
	if f.calls() != 1 {
		t.Fatalf("model calls = %d", f.calls())
	}
	f.mu.Lock()
	msgs := f.requests[0].Messages
	f.mu.Unlock()
	var lines []string
	for _, m := range msgs[1:] {
		lines = append(lines, m.Content)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		"[2026-10-06T07:32:05+00:00] Ann#", ": shall we meet at noon?",
		"[2026-10-06T07:33:05+00:00] Bob#", ": noon works",
		"[2026-10-06T07:34:05+00:00] Ann#", ": great",
		"[2026-10-06T07:42:05+00:00] Bob#", ": @bot what did we agree?",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("the model was not shown %q:\n%s", want, joined)
		}
	}
	if len(msgs) != 5 {
		t.Errorf("model saw %d messages, want system + 4", len(msgs))
	}
	if got := rec.all(); len(got) != 1 || got[0] != "pong" {
		t.Errorf("replies = %q", got)
	}
}

func TestGroupContextAddressedKeepsOnlyWhatIsForTheBot(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{GroupContext: GroupContextAddressed})
	rec := &recorder{}
	r.Handle(ctx, group(rec, "1", "Ann", "private banter", false))
	r.Handle(ctx, group(rec, "2", "Bob", "@bot hello", true))
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 || len(f.requests[0].Messages) != 2 {
		t.Fatalf("requests = %+v", f.requests)
	}
	if strings.Contains(f.requests[0].Messages[1].Content, "banter") {
		t.Error("an unaddressed message was kept although --group-context=addressed")
	}
}

func TestOnlyAllowedUsersAreRecorded(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{AllowedUsers: []string{"2"}})
	rec := &recorder{}
	r.Handle(ctx, group(rec, "1", "Stranger", "secret plans", false))
	r.Handle(ctx, group(rec, "2", "Bob", "@bot hi", true))
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.Contains(f.requests[0].Messages[1].Content, "secret plans") || len(f.requests[0].Messages) != 2 {
		t.Errorf("a user who is not allowed was recorded: %+v", f.requests[0].Messages)
	}
	if len(rec.all()) != 1 {
		t.Errorf("replies = %q (a stranger's chatter must not get a 'not allowed' reply either)", rec.all())
	}
}

func TestRecordingIgnoresEmptyMessagesAndTruncatesHugeOnes(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	r.Handle(ctx, group(rec, "1", "Ann", "   ", false))
	attach := group(rec, "1", "Ann", "", false)
	attach.HasAttachment = true
	r.Handle(ctx, attach)
	r.Handle(ctx, group(rec, "1", "Ann", strings.Repeat("ก", maxRecordRunes+500), false))
	r.Handle(ctx, group(rec, "2", "Bob", "@bot?", true))
	f.mu.Lock()
	defer f.mu.Unlock()
	msgs := f.requests[0].Messages
	if len(msgs) != 3 {
		t.Fatalf("model saw %d messages, want system + the long one + the question", len(msgs))
	}
	if n := len([]rune(msgs[1].Content)); n > maxRecordRunes+80 || !strings.HasSuffix(msgs[1].Content, "…") {
		t.Errorf("the long message was not truncated (%d characters)", n)
	}
}

func TestRecordingDoesNotWaitForARunningRequestAndIsNeverRefused(t *testing.T) {
	gate := make(chan struct{})
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		<-gate
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "slow answer"}}, nil
	}}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	done := make(chan struct{})
	go func() { r.Handle(ctx, group(rec, "2", "Bob", "@bot slow one", true)); close(done) }()
	deadline := time.Now().Add(3 * time.Second)
	for f.calls() < 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	finished := make(chan struct{})
	go func() { // far more than the chat's request queue holds
		for i := 0; i < 30; i++ {
			r.Handle(ctx, group(rec, "1", "Ann", fmt.Sprintf("chatter %d", i), false))
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("collecting messages blocked behind the running request")
	}
	close(gate)
	<-done

	r.Handle(ctx, group(rec, "2", "Bob", "@bot next", true))
	f.mu.Lock()
	last := f.requests[len(f.requests)-1].Messages
	f.mu.Unlock()
	if len(last) != 1+1+30+1+1 { // system, first question, 30 chatter, answer, next question
		t.Errorf("model saw %d messages", len(last))
	}
	if got := rec.all(); len(got) != 2 || strings.Contains(strings.Join(got, ""), "still working") {
		t.Errorf("replies = %q", got)
	}
}

func TestRecordingIsRateLimitedPerChat(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	for i := 0; i < recordsPerMinute+50; i++ {
		r.Handle(ctx, group(rec, "1", "Spammer", fmt.Sprintf("spam %d", i), false))
	}
	var count int
	_ = r.sessions.Record("tg", "g1", func(s *session.Session) error { count, _ = s.Count(ctx); return nil })
	if count != recordsPerMinute {
		t.Errorf("kept %d messages of a flood, want %d", count, recordsPerMinute)
	}
	// another group is unaffected
	other := group(rec, "1", "Ann", "hello", false)
	other.ChatID = "g2"
	r.Handle(ctx, other)
	_ = r.sessions.Record("tg", "g2", func(s *session.Session) error { count, _ = s.Count(ctx); return nil })
	if count != 1 {
		t.Errorf("the other group has %d messages", count)
	}
}

func TestACollectingGroupIsCompactedInTheBackground(t *testing.T) {
	var mu sync.Mutex
	var summaries int
	f := &fakeLLM{reply: func(req llm.Request) (*llm.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		summaries++
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "- Ann and Bob talked about lunch"}}, nil
	}}
	r := newRouterWith(t, f, Config{CompactAfter: 10}, agent.Config{Model: "m", CompactAfter: 10, CompactKeep: 4})
	rec := &recorder{}
	for i := 0; i < 14; i++ {
		who := []string{"Ann", "Bob"}[i%2]
		r.Handle(ctx, group(rec, who, who, fmt.Sprintf("line %d", i), false))
	}
	r.Wait()
	mu.Lock()
	n := summaries
	mu.Unlock()
	if n != 1 {
		t.Fatalf("summaries = %d, want exactly one (a retry gate prevents a storm)", n)
	}
	_ = r.sessions.Record("tg", "g1", func(s *session.Session) error {
		sum, _ := s.Summary(ctx)
		count, _ := s.Count(ctx)
		if !strings.Contains(sum, "lunch") || count > 6 {
			t.Errorf("summary = %q, remaining messages = %d", sum, count)
		}
		return nil
	})
	if len(rec.all()) != 0 {
		t.Errorf("compacting must not send anything to the group: %q", rec.all())
	}
}

func TestAGroupThatCannotBeCompactedStaysBounded(t *testing.T) {
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) { return nil, errors.New("model is down") }}
	r := newRouterWith(t, f, Config{CompactAfter: 10}, agent.Config{Model: "m", CompactAfter: 10, CompactKeep: 4})
	rec := &recorder{}
	for i := 0; i < 400; i++ {
		r.Handle(ctx, group(rec, "1", "Ann", fmt.Sprintf("line %d", i), false))
	}
	r.Wait()
	var count int
	_ = r.sessions.Record("tg", "g1", func(s *session.Session) error { count, _ = s.Count(ctx); return nil })
	if count > 5*10+1 {
		t.Errorf("the group grew to %d messages although compaction fails", count)
	}
}

func TestPrivateMessagesCarryTheirTimeFromTheChannel(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	in := msg(rec, "hi")
	in.ReceivedAt = time.Date(1997, 7, 16, 18, 20, 44, 0, time.UTC)
	r.Handle(ctx, in)
	f.mu.Lock()
	first := f.requests[0].Messages[1].Content
	f.mu.Unlock()
	if first != "[1997-07-16T18:20:44+00:00] hi" {
		t.Errorf("message = %q", first)
	}
	// without a platform time the moment of arrival is used
	before := time.Now()
	r.Handle(ctx, msg(rec, "again"))
	f.mu.Lock()
	last := f.requests[len(f.requests)-1].Messages
	f.mu.Unlock()
	got := last[len(last)-1].Content
	ts := strings.TrimPrefix(strings.SplitN(got, "]", 2)[0], "[")
	parsed, err := time.Parse(time.RFC3339, ts)
	if err != nil || parsed.Before(before.Truncate(time.Second)) || parsed.After(time.Now().Add(time.Second)) {
		t.Errorf("arrival time %q (%v)", ts, err)
	}
}

// storedTexts returns the user and assistant texts of a chat, oldest first.
func storedTexts(t *testing.T, r *Router, ch, chat string) []string {
	t.Helper()
	var out []string
	_ = r.sessions.Record(ch, chat, func(s *session.Session) error {
		turns, _ := s.Recent(ctx, 1000)
		for _, tn := range turns {
			out = append(out, tn.Role+":"+tn.Text)
		}
		return nil
	})
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestAcceptedIsCalledExactlyOnceOnEveryPath(t *testing.T) {
	f := &fakeLLM{}
	r := newRouter(t, f, Config{AllowedUsers: []string{"u1"}, RateLimit: 3})
	for name, mk := range map[string]func(*recorder) channel.Incoming{
		"private message":      func(rec *recorder) channel.Incoming { return msg(rec, "hello") },
		"command":              func(rec *recorder) channel.Incoming { return msg(rec, "/help") },
		"reset":                func(rec *recorder) channel.Incoming { return msg(rec, "/reset") },
		"empty":                func(rec *recorder) channel.Incoming { return msg(rec, "  ") },
		"attachment only":      func(rec *recorder) channel.Incoming { in := msg(rec, ""); in.HasAttachment = true; return in },
		"group chatter":        func(rec *recorder) channel.Incoming { return group(rec, "u1", "Ann", "talk", false) },
		"group chatter, empty": func(rec *recorder) channel.Incoming { return group(rec, "u1", "Ann", " ", false) },
		"not allowed":          func(rec *recorder) channel.Incoming { in := msg(rec, "hi"); in.UserID = "stranger"; return in },
		"stranger in a group":  func(rec *recorder) channel.Incoming { return group(rec, "stranger", "Eve", "hi", false) },
	} {
		var calls int
		rec := &recorder{}
		in := mk(rec)
		in.Accepted = func() { calls++ }
		r.Handle(ctx, in)
		if calls != 1 {
			t.Errorf("%s: Accepted called %d times, want 1", name, calls)
		}
	}
	// rate limited (the limit is 3 per minute per user)
	rec := &recorder{}
	for i := 0; i < 5; i++ {
		var calls int
		in := msg(rec, "again")
		in.Accepted = func() { calls++ }
		r.Handle(ctx, in)
		if calls != 1 {
			t.Errorf("message %d: Accepted called %d times", i, calls)
		}
	}
	// a message without a responder is dropped, but still released
	var calls int
	r.Handle(ctx, channel.Incoming{Channel: "test", ChatID: "x", Text: "orphan", Accepted: func() { calls++ }})
	if calls != 1 {
		t.Errorf("no responder: Accepted called %d times", calls)
	}
}

func TestAcceptedComesBeforeTheSlowWork(t *testing.T) {
	gate := make(chan struct{})
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		<-gate
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "late"}}, nil
	}}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	accepted := make(chan struct{})
	in := msg(rec, "question")
	in.Accepted = func() { close(accepted) }
	done := make(chan struct{})
	go func() { r.Handle(ctx, in); close(done) }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("the message was not accepted while the model was still busy")
	}
	select {
	case <-done:
		t.Fatal("Handle returned before the answer")
	default:
	}
	if got := storedTexts(t, r, "test", "c1"); len(got) == 0 || !strings.HasSuffix(got[0], "question") {
		t.Errorf("the message must be stored by the time it is accepted: %v", got)
	}
	close(gate)
	<-done
}

func TestStoredInArrivalOrderEvenWhenTheChatIsBusy(t *testing.T) {
	gate := make(chan struct{})
	var first sync.Once
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		first.Do(func() { <-gate }) // only the first question is slow
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil
	}}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	var wg sync.WaitGroup
	send := func(in channel.Incoming) {
		wg.Add(1)
		go func() { defer wg.Done(); r.Handle(ctx, in) }()
	}
	count := func() int {
		n := 0
		_ = r.sessions.Record("tg", "g1", func(s *session.Session) error { n, _ = s.Count(ctx); return nil })
		return n
	}
	send(group(rec, "2", "Bob", "Q1", true))
	waitFor(t, func() bool { return f.calls() == 1 }) // Q1 is with the model
	chatter := group(rec, "1", "Ann", "chatter", false)
	r.Handle(ctx, chatter)
	send(group(rec, "2", "Bob", "Q2", true))
	waitFor(t, func() bool { return count() == 3 })
	close(gate)
	wg.Wait()
	got := storedTexts(t, r, "tg", "g1")
	want := []string{"user:Q1", "user:chatter", "user:Q2"}
	var users []string
	for _, g := range got {
		if strings.HasPrefix(g, "user:") {
			users = append(users, g)
		}
	}
	if strings.Join(users, ",") != strings.Join(want, ",") {
		t.Errorf("stored order = %v", got)
	}
}

func TestTheModelIsToldWhichMessageItIsAnswering(t *testing.T) {
	gate := make(chan struct{})
	var calls sync.Mutex
	var seen [][]llm.Message
	var first sync.Once
	f := &fakeLLM{reply: func(req llm.Request) (*llm.Response, error) {
		calls.Lock()
		seen = append(seen, req.Messages)
		calls.Unlock()
		first.Do(func() { <-gate })
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "answer"}}, nil
	}}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	var wg sync.WaitGroup
	send := func(in channel.Incoming) {
		wg.Add(1)
		go func() { defer wg.Done(); r.Handle(ctx, in) }()
	}
	count := func() int {
		n := 0
		_ = r.sessions.Record("tg", "g1", func(s *session.Session) error { n, _ = s.Count(ctx); return nil })
		return n
	}
	send(group(rec, "2", "Bob", "Q0 first question", true))
	waitFor(t, func() bool { return f.calls() == 1 })
	send(group(rec, "3", "Cy", "Q1 second question", true))
	waitFor(t, func() bool { return count() == 2 })
	time.Sleep(50 * time.Millisecond) // Q1 is queued behind Q0
	r.Handle(ctx, group(rec, "1", "Ann", "some chatter in between", false))
	send(group(rec, "2", "Bob", "Q2 third question", true))
	waitFor(t, func() bool { return count() == 4 })
	time.Sleep(50 * time.Millisecond)
	close(gate)
	wg.Wait()

	calls.Lock()
	defer calls.Unlock()
	if len(seen) != 3 {
		t.Fatalf("model calls = %d, want 3", len(seen))
	}
	last := func(msgs []llm.Message) llm.Message { return msgs[len(msgs)-1] }
	if m := last(seen[0]); m.Role == llm.RoleSystem {
		t.Errorf("nothing came after Q0, no note expected: %q", m.Content)
	}
	// the run for Q1 sees the chatter and Q2 as well: it is told what to answer
	note := last(seen[1])
	if note.Role != llm.RoleSystem || !strings.Contains(note.Content, "Q1 second question") ||
		!strings.Contains(note.Content, "answered separately") {
		t.Errorf("run for Q1: last message = %+v", note)
	}
	if strings.Contains(note.Content, "Q2") {
		t.Error("the note must name only the message being answered")
	}
	// the run for the newest message needs no note
	if m := last(seen[2]); m.Role == llm.RoleSystem {
		t.Errorf("the newest message needs no note: %q", m.Content)
	}
	if got := rec.all(); len(got) != 3 {
		t.Errorf("replies = %q, want three (one per question, none for the chatter)", got)
	}
}

func TestResetOnlyForgetsWhatCameBeforeIt(t *testing.T) {
	gate := make(chan struct{})
	var first sync.Once
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		first.Do(func() { <-gate })
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil
	}}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	var wg sync.WaitGroup
	send := func(in channel.Incoming) {
		wg.Add(1)
		go func() { defer wg.Done(); r.Handle(ctx, in) }()
	}
	send(msg(rec, "before the reset")) // running, so the reset below has to wait for its turn
	waitFor(t, func() bool { return f.calls() == 1 })
	send(msg(rec, "/reset"))
	time.Sleep(100 * time.Millisecond)
	send(msg(rec, "sent after the reset was asked for")) // waits for its turn like the reset does
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	waitFor(t, func() bool { return len(rec.all()) >= 3 })
	got := strings.Join(storedTexts(t, r, "test", "c1"), "|")
	if strings.Contains(got, "before the reset") {
		t.Errorf("the reset did not forget the earlier message: %s", got)
	}
	if !strings.Contains(got, "sent after the reset was asked for") {
		t.Errorf("the reset wiped a message that came after it: %s", got)
	}
}

func TestABusyChatStillKeepsTheMessage(t *testing.T) {
	gate := make(chan struct{})
	var first sync.Once
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		first.Do(func() { <-gate })
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil
	}}
	r := newRouter(t, f, Config{})
	rec := &recorder{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // fills the queue: one running, three waiting
		in := msg(rec, fmt.Sprintf("queued %d", i))
		wg.Add(1)
		go func() { defer wg.Done(); r.Handle(ctx, in) }()
		time.Sleep(30 * time.Millisecond)
	}
	overflow := &recorder{}
	r.Handle(ctx, msg(overflow, "one too many"))
	if got := overflow.all(); len(got) != 1 || !strings.Contains(got[0], "still working") {
		t.Errorf("overflow reply = %q", got)
	}
	if got := strings.Join(storedTexts(t, r, "test", "c1"), "|"); !strings.Contains(got, "one too many") {
		t.Errorf("a message that got the 'busy' answer was not kept: %s", got)
	}
	close(gate)
	wg.Wait()
}
