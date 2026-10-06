package router

import (
	"context"
	"errors"
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := session.NewManager(t.TempDir(), 8, 4)
	t.Cleanup(func() { sm.Close() })
	ag := agent.New(agent.Config{Model: "m", CompactAfter: 200, CompactKeep: 20}, p, nil, log)
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
	if len(second) != 4 || second[1].Content != "ping" || second[2].Content != "pong" {
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
	if u := f.requests[0].Messages[1].Content; u != "Ann: chatter between humans" {
		t.Errorf("group message should carry the sender name, got %q", u)
	}
	f.mu.Unlock()

	r2 := newRouter(t, f, Config{GroupReply: GroupReplyAll})
	in.Addressed = false
	r2.Handle(ctx, in)
	if f.calls() != 2 {
		t.Error("group-reply=all must answer unaddressed messages")
	}
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
	if got := rec.all(); len(got) != 1 || !strings.Contains(got[0], "attachments") {
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
