package router

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/ntfy"
	"github.com/authapon/jannyq/internal/trigger"
)

type ntfyServer struct {
	mu   sync.Mutex
	got  []map[string]any
	code int
	auth []string
}

func (s *ntfyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = append(s.auth, r.Header.Get("Authorization"))
	if s.code != 0 {
		w.WriteHeader(s.code)
		return
	}
	s.got = append(s.got, m)
}

func (s *ntfyServer) messages() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.got...)
}

func ntfyRouter(t *testing.T, saved string) (*Router, *fakeNotifier, *ntfyServer, *fakeLLM) {
	t.Helper()
	srv := &ntfyServer{}
	hs := httptest.NewServer(srv)
	t.Cleanup(hs.Close)
	f := &fakeLLM{reply: func(llm.Request) (*llm.Response, error) {
		return &llm.Response{Message: llm.Message{Role: llm.RoleAssistant, Content: "Ann, time to call Peter."}}, nil
	}}
	r, n := triggerRouter(t, f, Config{}, nil)
	r.SetNtfy(&ntfy.Client{BaseURL: hs.URL, Token: "tk_secret", TopicPrefix: "jq-"}, func(_ context.Context, ch, user string) (trigger.Prefs, error) {
		if ch == "telegram" && user == "5" {
			return trigger.Prefs{Channel: ch, UserID: user, NtfyTopic: saved}, nil
		}
		return trigger.Prefs{}, nil
	})
	return r, n, srv, f
}

func TestNtfyOnlySkipsTheChatButKeepsTheHistory(t *testing.T) {
	r, n, srv, _ := ntfyRouter(t, "ann-phone")
	tr := remind("Call Peter")
	tr.Notify, tr.Title, tr.Priority = trigger.NotifyNtfy, "Peter", 4
	if err := r.RunTrigger(ctx, tr, 0); err != nil {
		t.Fatal(err)
	}
	if got := n.rec.all(); len(got) != 0 {
		t.Errorf("the chat must not be told: %q", got)
	}
	got := srv.messages()
	if len(got) != 1 || got[0]["topic"] != "jq-ann-phone" || got[0]["message"] != "Ann, time to call Peter." || got[0]["title"] != "Peter" || got[0]["priority"] != float64(4) {
		t.Fatalf("ntfy got %v", got)
	}
	if srv.auth[0] != "Bearer tk_secret" {
		t.Errorf("auth = %q", srv.auth[0])
	}
	_, texts := stored(t, r)
	if len(texts) == 0 || texts[len(texts)-1] != "Ann, time to call Peter." {
		t.Errorf("the reminder is part of the conversation: %q", texts)
	}
}

func TestNtfyBothAndTheTopicOfTheTrigger(t *testing.T) {
	r, n, srv, _ := ntfyRouter(t, "ann-phone")
	tr := remind("Call Peter")
	tr.Notify, tr.NtfyTopic = trigger.NotifyBoth, "work-alerts"
	if err := r.RunTrigger(ctx, tr, 0); err != nil {
		t.Fatal(err)
	}
	if got := n.rec.all(); len(got) != 1 || got[0] != "Ann, time to call Peter." {
		t.Errorf("chat: %q", got)
	}
	if got := srv.messages(); len(got) != 1 || got[0]["topic"] != "jq-work-alerts" {
		t.Errorf("the trigger's own topic must win: %v", got)
	}
}

func TestNtfyFailureFallsBackToTheChatWithANote(t *testing.T) {
	for name, tc := range map[string]struct {
		saved string
		code  int
		want  string
	}{
		"refused":    {"ann-phone", 403, "refused"},
		"no topic":   {"", 0, "no ntfy topic"},
		"server 500": {"ann-phone", 500, "ntfy"},
	} {
		r, n, srv, _ := ntfyRouter(t, tc.saved)
		srv.code = tc.code
		tr := remind("Call Peter")
		tr.Notify = trigger.NotifyNtfy
		if err := r.RunTrigger(ctx, tr, 0); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := n.rec.all()
		if len(got) != 1 || !strings.HasPrefix(got[0], "Ann, time to call Peter.") || !strings.Contains(got[0], "could not send this to your phone") || !strings.Contains(got[0], tc.want) {
			t.Errorf("%s: chat got %q", name, got)
		}
		for _, bad := range []string{"tk_secret", "127.0.0.1", "ann-phone"} {
			if len(got) == 1 && strings.Contains(got[0], bad) {
				t.Errorf("%s: the note leaks %q: %q", name, bad, got[0])
			}
		}
	}
}

func TestNtfyIsOffWhenNotSetUp(t *testing.T) {
	r, n, _, _ := ntfyRouter(t, "ann-phone")
	r.SetNtfy(nil, nil)
	tr := remind("Call Peter")
	tr.Notify = trigger.NotifyNtfy
	if err := r.RunTrigger(ctx, tr, 0); err != nil {
		t.Fatal(err)
	}
	if got := n.rec.all(); len(got) != 1 || !strings.Contains(got[0], "not set up") {
		t.Errorf("chat: %q", got)
	}
}

func TestBothSucceedsWhenOnlyTheChatIsUnreachable(t *testing.T) {
	r, _, srv, _ := ntfyRouter(t, "ann-phone")
	r.SetNotifiers(map[string]channel.Notifier{"telegram": notifierFor{&failingResponder{}}})
	tr := remind("Call Peter")
	tr.Notify = trigger.NotifyBoth
	if err := r.RunTrigger(ctx, tr, 0); err != nil {
		t.Fatalf("ntfy has it, so it is delivered: %v", err)
	}
	if len(srv.messages()) != 1 {
		t.Error("ntfy must have been used")
	}
	// when neither works, it is an error (the scheduler retries)
	srv.code = 500
	if err := r.RunTrigger(ctx, tr, 0); err == nil {
		t.Error("nothing was delivered")
	}
}

func TestNtfyOnlyNeedsNoChannelButFallbackDoes(t *testing.T) {
	r, _, srv, _ := ntfyRouter(t, "ann-phone")
	r.SetNotifiers(map[string]channel.Notifier{}) // the channel is not running
	tr := remind("Call Peter")
	tr.Notify = trigger.NotifyNtfy
	if err := r.RunTrigger(ctx, tr, 0); err != nil {
		t.Fatalf("ntfy only does not need the channel: %v", err)
	}
	if len(srv.messages()) != 1 {
		t.Error("not pushed")
	}
	srv.code = 500
	if err := r.RunTrigger(ctx, tr, 0); err == nil {
		t.Error("ntfy failed and there is no chat to fall back to: that is an error")
	}
	// chat or both need the channel before the model is asked
	tr.Notify = trigger.NotifyBoth
	if err := r.RunTrigger(ctx, tr, 0); err == nil || !strings.Contains(err.Error(), "telegram") {
		t.Errorf("err = %v", err)
	}
}

func TestPlainRemindersAndTasksGoThroughNtfyToo(t *testing.T) {
	r, n, srv, f := ntfyRouter(t, "ann-phone")
	r.cfg.TriggerPlain = true
	r.Handle(ctx, channel.Incoming{Channel: "telegram", ChatID: "42", UserID: "5", UserName: "Ann", Text: "hello", Addressed: true, Responder: &recorder{}})
	before := f.calls()
	tr := remind("Call Peter")
	tr.Notify = trigger.NotifyNtfy
	if err := r.RunTrigger(ctx, tr, 0); err != nil {
		t.Fatal(err)
	}
	got := srv.messages()
	if len(got) != 1 || got[0]["message"] != "⏰ Reminder: Call Peter" || f.calls() != before || len(n.rec.all()) != 0 {
		t.Errorf("plain: %v, %d model calls, chat %q", got, f.calls(), n.rec.all())
	}
	if _, texts := stored(t, r); len(texts) == 0 || texts[len(texts)-1] != "⏰ Reminder: Call Peter" {
		t.Errorf("history: %q", texts)
	}

	r, _, srv, _ = ntfyRouter(t, "ann-phone")
	task := remind("Find the news")
	task.Mode, task.Notify, task.Title = trigger.ModeTask, trigger.NotifyNtfy, ""
	if err := r.RunTrigger(ctx, task, 0); err != nil {
		t.Fatal(err)
	}
	got = srv.messages()
	if len(got) != 1 || got[0]["title"] != "Scheduled task" {
		t.Errorf("task: %v", got)
	}
}
