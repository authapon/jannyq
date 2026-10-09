package router

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/agent"
	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/i18n"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/tool"
	"github.com/authapon/jannyq/internal/trigger"
)

// fakeNotifier delivers to a recorder and remembers which chat was asked for.
type fakeNotifier struct {
	rec  *recorder
	chat string
	err  error
}

func (f *fakeNotifier) ResponderFor(chatID string, _ bool) (channel.Responder, error) {
	f.chat = chatID
	return f.rec, f.err
}

func triggerRouter(t *testing.T, p llm.Provider, cfg Config, tools *tool.Registry) (*Router, *fakeNotifier) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := session.NewManager(t.TempDir(), 8, 4)
	t.Cleanup(func() { sm.Close() })
	ag := agent.New(agent.Config{Model: "m", Location: time.UTC, BotName: "Janny"}, p, tools, log)
	r := New(cfg, sm, ag, i18n.New("en"), log)
	n := &fakeNotifier{rec: &recorder{}}
	r.SetNotifiers(map[string]channel.Notifier{"telegram": n})
	return r, n
}

func remind(text string) trigger.Trigger {
	at := time.Now().Add(-time.Second)
	return trigger.Trigger{ID: 7, Channel: "telegram", ChatID: "42", OwnerID: "5", OwnerName: "Ann", Mode: trigger.ModeRemind,
		Text: text, At: at, Next: at, Zone: "Asia/Bangkok", Status: trigger.StatusActive}
}

func stored(t *testing.T, r *Router) (roles, texts []string) {
	t.Helper()
	_ = r.sessions.With(ctx, "telegram", "42", func(s *session.Session) error {
		st, _ := s.Messages(ctx)
		for _, m := range st {
			roles = append(roles, string(m.Message.Role))
			texts = append(texts, m.Message.Content)
		}
		return nil
	})
	return
}

func TestAReminderIsGivenByTheModelInTheChat(t *testing.T) {
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "Ann, it's 3 pm: time to call Peter about the quote."}}, nil
	}}
	reg := tool.NewRegistry()
	reg.Register(&echoTool{})
	r, n := triggerRouter(t, f, Config{}, reg)
	// some earlier conversation, to be seen by the model
	r.Handle(ctx, channel.Incoming{Channel: "telegram", ChatID: "42", UserID: "5", UserName: "Ann", Text: "Peter is the supplier", Addressed: true, Responder: &recorder{}})
	before := f.calls()

	if err := r.RunTrigger(ctx, remind("Call Peter about the quote"), 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := n.rec.all(); len(got) != 1 || got[0] != "Ann, it's 3 pm: time to call Peter about the quote." || n.chat != "42" {
		t.Fatalf("delivered %q to chat %q", got, n.chat)
	}
	f.mu.Lock()
	req := f.requests[before]
	f.mu.Unlock()
	if len(req.Tools) != 0 {
		t.Error("a reminder needs no tools")
	}
	note := req.Messages[len(req.Messages)-1].Content
	for _, want := range []string{"Call Peter about the quote", "Ann", "language of the conversation", "add no new facts"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note to the model lacks %q:\n%s", want, note)
		}
	}
	if strings.Contains(note, "It is late") {
		t.Error("a reminder two seconds late is not late")
	}
	// the model saw the chat so far
	seen := ""
	for _, m := range req.Messages {
		seen += m.Content + "\n"
	}
	if !strings.Contains(seen, "Peter is the supplier") || !strings.Contains(seen, "[Scheduled reminder #7, set up by Ann] Call Peter about the quote") {
		t.Errorf("the model did not see the conversation and the reminder:\n%s", seen)
	}
	// and what was said is in the history, as part of the conversation
	roles, texts := stored(t, r)
	n2 := len(texts)
	if n2 < 2 || roles[n2-2] != "user" || !strings.HasPrefix(texts[n2-2], "[Scheduled reminder #7") || roles[n2-1] != "assistant" ||
		texts[n2-1] != "Ann, it's 3 pm: time to call Peter about the quote." {
		t.Errorf("history = %q %q", roles, texts)
	}
}

type echoTool struct{}

func (*echoTool) Name() string        { return "echo" }
func (*echoTool) Description() string { return "echo" }
func (*echoTool) Parameters() []byte  { return []byte(`{"type":"object"}`) }
func (*echoTool) Execute(context.Context, tool.CallContext, []byte) (string, error) {
	return "echoed", nil
}

func TestAReminderIsSentAsWrittenWhenTheModelFails(t *testing.T) {
	for name, f := range map[string]*fakeLLM{
		"error": {reply: func(llm.Request) (*llm.Response, error) { return nil, errors.New("model down") }},
		"empty": {reply: func(llm.Request) (*llm.Response, error) {
			return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "  "}}, nil
		}},
		"slow": {reply: func(llm.Request) (*llm.Response, error) {
			time.Sleep(2 * time.Second)
			return nil, errors.New("too late")
		}},
	} {
		r, n := triggerRouter(t, f, Config{TriggerRemindTimeout: 100 * time.Millisecond}, nil)
		start := time.Now()
		if err := r.RunTrigger(ctx, remind("Call Peter about the quote"), time.Second); err != nil {
			t.Fatalf("%s: a reminder must not fail because of the model: %v", name, err)
		}
		if got := n.rec.all(); len(got) != 1 || got[0] != "⏰ Reminder: Call Peter about the quote" {
			t.Errorf("%s: delivered %q", name, got)
		}
		if name == "slow" && time.Since(start) > time.Second {
			t.Errorf("slow: the model was waited for %v", time.Since(start))
		}
		if _, texts := stored(t, r); len(texts) == 0 || texts[len(texts)-1] != "⏰ Reminder: Call Peter about the quote" {
			t.Errorf("%s: the plain reminder is not in the history: %q", name, texts)
		}
	}
}

func TestALateReminderSaysSo(t *testing.T) {
	var note string
	f := &fakeLLM{reply: func(req llm.Request) (*llm.Response, error) {
		note = req.Messages[len(req.Messages)-1].Content
		return nil, errors.New("down")
	}}
	r, n := triggerRouter(t, f, Config{}, nil)
	tr := remind("Take the medicine")
	tr.At = time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC) // 15:00 in Bangkok
	tr.Next = tr.At
	if err := r.RunTrigger(ctx, tr, 3*time.Hour); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "It is late") || !strings.Contains(note, "2026-10-09 15:00") {
		t.Errorf("the model is not told that it is late:\n%s", note)
	}
	if got := n.rec.all(); len(got) != 1 || got[0] != "⏰ Reminder (it was due at 2026-10-09 15:00): Take the medicine" {
		t.Errorf("delivered %q", got)
	}
}

func TestATaskGetsToolsAndReportsItsResult(t *testing.T) {
	var offered int
	f := &fakeLLM{reply: func(req llm.Request) (*llm.Response, error) {
		offered = len(req.Tools)
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "Here is today's news: ..."}}, nil
	}}
	reg := tool.NewRegistry()
	reg.Register(&echoTool{})
	r, n := triggerRouter(t, f, Config{}, reg)
	tr := remind("Find interesting news for me")
	tr.Mode = trigger.ModeTask
	if err := r.RunTrigger(ctx, tr, 0); err != nil {
		t.Fatal(err)
	}
	if offered != 1 {
		t.Errorf("a task has tools: %d offered", offered)
	}
	if got := n.rec.all(); len(got) != 1 || got[0] != "Here is today's news: ..." {
		t.Errorf("delivered %q", got)
	}
	// a task that fails says so, and fails (it is not turned into a plain reminder)
	f2 := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) { return nil, errors.New("model down") }}
	r2, n2 := triggerRouter(t, f2, Config{}, reg)
	tr.Title = "news"
	if err := r2.RunTrigger(ctx, tr, 0); err == nil {
		t.Error("a failed task is an error")
	}
	if got := n2.rec.all(); len(got) != 1 || !strings.Contains(got[0], `"news"`) {
		t.Errorf("the user should be told: %q", got)
	}
}

func TestATriggerOfSomeoneNotAllowedIsDisabled(t *testing.T) {
	f := &fakeLLM{}
	r, n := triggerRouter(t, f, Config{AllowedUsers: []string{"telegram:9"}}, nil)
	err := r.RunTrigger(ctx, remind("x"), 0)
	var dis *trigger.DisableError
	if !errors.As(err, &dis) {
		t.Fatalf("err = %v", err)
	}
	if f.calls() != 0 || len(n.rec.all()) != 0 {
		t.Error("nothing may be sent for someone who is not allowed")
	}
	// a group on the list covers its members
	r, n = triggerRouter(t, f, Config{AllowedGroups: []string{"telegram:42"}}, nil)
	tr := remind("x")
	tr.IsGroup = true
	if err := r.RunTrigger(ctx, tr, 0); err != nil || len(n.rec.all()) != 1 {
		t.Errorf("group trigger: %v %q", err, n.rec.all())
	}
}

func TestATriggerNeedsARunningChannel(t *testing.T) {
	r, _ := triggerRouter(t, &fakeLLM{}, Config{}, nil)
	tr := remind("x")
	tr.Channel = "line"
	if err := r.RunTrigger(ctx, tr, 0); err == nil || !strings.Contains(err.Error(), "line") {
		t.Errorf("err = %v", err)
	}
	r, n := triggerRouter(t, &fakeLLM{}, Config{}, nil)
	n.err = errors.New("cannot reach that chat")
	if err := r.RunTrigger(ctx, remind("x"), 0); err == nil {
		t.Error("a chat that cannot be reached is an error")
	}
	// delivery that fails is reported, so that the scheduler records it
	r, n = triggerRouter(t, &fakeLLM{}, Config{}, nil)
	n.rec = &recorder{}
	failing := &failingResponder{}
	r.SetNotifiers(map[string]channel.Notifier{"telegram": notifierFor{failing}})
	if err := r.RunTrigger(ctx, remind("x"), 0); err == nil || !strings.Contains(err.Error(), "deliver") {
		t.Errorf("err = %v", err)
	}
}

type failingResponder struct{}

func (failingResponder) Send(context.Context, string) error { return errors.New("blocked by the user") }
func (failingResponder) Typing(context.Context) error       { return nil }

type notifierFor struct{ r channel.Responder }

func (n notifierFor) ResponderFor(string, bool) (channel.Responder, error) { return n.r, nil }

func TestRemindersShowOnlyTheLatestMessages(t *testing.T) {
	var seen []llm.Message
	f := &fakeLLM{reply: func(req llm.Request) (*llm.Response, error) {
		seen = req.Messages
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "ok"}}, nil
	}}
	r, _ := triggerRouter(t, f, Config{TriggerHistory: 4}, nil)
	_ = r.sessions.With(ctx, "telegram", "42", func(s *session.Session) error {
		for i := 0; i < 30; i++ {
			_ = s.Append(ctx, llm.Message{Role: llm.RoleUser, Content: "old question"}, llm.Message{Role: llm.RoleAssistant, Content: "old answer"})
		}
		return nil
	})
	if err := r.RunTrigger(ctx, remind("Call Peter"), 0); err != nil {
		t.Fatal(err)
	}
	if len(seen) > 4+3 { // the system prompt, at most 4 messages, the note
		t.Errorf("the model was shown %d messages", len(seen))
	}
}
