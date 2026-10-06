package telegram

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/channel"
)

const token = "123:SECRET"

func newChannel(base string) *Channel {
	c := New(Config{Token: token, APIBase: base, PollTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.botID, c.botUser = 99, "JannyBot"
	return c
}

func TestConvert(t *testing.T) {
	c := newChannel("http://unused")
	user := &tgUser{ID: 5, FirstName: "Ann", LastName: "Lee"}
	chat := func(typ string) tgChat { return tgChat{ID: -1001, Type: typ} }

	type want struct {
		ok, group, addressed bool
		text                 string
	}
	for name, tc := range map[string]struct {
		m    *tgMessage
		want want
	}{
		"private":               {&tgMessage{From: user, Chat: tgChat{ID: 5, Type: "private"}, Text: "hello"}, want{true, false, true, "hello"}},
		"group chatter":         {&tgMessage{From: user, Chat: chat("supergroup"), Text: "hello all"}, want{true, true, false, "hello all"}},
		"group mention":         {&tgMessage{From: user, Chat: chat("group"), Text: "@jannybot what time is it?"}, want{true, true, true, "what time is it?"}},
		"mention mid":           {&tgMessage{From: user, Chat: chat("group"), Text: "hey @JannyBot, help"}, want{true, true, true, "hey , help"}},
		"other bot mention":     {&tgMessage{From: user, Chat: chat("group"), Text: "@OtherBot hi"}, want{true, true, false, "@OtherBot hi"}},
		"reply to bot":          {&tgMessage{From: user, Chat: chat("group"), Text: "and then?", ReplyToMessage: &tgMessage{From: &tgUser{ID: 99}}}, want{true, true, true, "and then?"}},
		"reply to human":        {&tgMessage{From: user, Chat: chat("group"), Text: "agree", ReplyToMessage: &tgMessage{From: &tgUser{ID: 7}}}, want{true, true, false, "agree"}},
		"command in group":      {&tgMessage{From: user, Chat: chat("group"), Text: "/reset"}, want{true, true, true, "/reset"}},
		"command for us":        {&tgMessage{From: user, Chat: chat("group"), Text: "/reset@JannyBot now"}, want{true, true, true, "/reset now"}},
		"command for other bot": {&tgMessage{From: user, Chat: chat("group"), Text: "/reset@OtherBot"}, want{false, false, false, ""}},
		"caption":               {&tgMessage{From: user, Chat: tgChat{ID: 5, Type: "private"}, Caption: "look", Photo: json.RawMessage(`[{}]`)}, want{true, false, true, "look"}},
		"channel post":          {&tgMessage{From: user, Chat: chat("channel"), Text: "x"}, want{false, false, false, ""}},
		"from bot":              {&tgMessage{From: &tgUser{ID: 1, IsBot: true}, Chat: tgChat{ID: 5, Type: "private"}, Text: "x"}, want{false, false, false, ""}},
		"no sender":             {&tgMessage{Chat: tgChat{ID: 5, Type: "private"}, Text: "x"}, want{false, false, false, ""}},
	} {
		in, ok := c.convert(tc.m)
		if ok != tc.want.ok {
			t.Errorf("%s: ok = %v", name, ok)
			continue
		}
		if !ok {
			continue
		}
		if in.Text != tc.want.text || in.IsGroup != tc.want.group || in.Addressed != tc.want.addressed {
			t.Errorf("%s: got text=%q group=%v addressed=%v", name, in.Text, in.IsGroup, in.Addressed)
		}
	}

	in, _ := c.convert(&tgMessage{From: user, Chat: tgChat{ID: 5, Type: "private"}, Photo: json.RawMessage(`[{}]`)})
	if !in.HasAttachment || in.Text != "" || in.UserName != "Ann Lee" || in.UserID != "5" || in.ChatID != "5" {
		t.Errorf("attachment message: %+v", in)
	}
	in, _ = c.convert(&tgMessage{From: &tgUser{ID: 6, Username: "bob"}, Chat: tgChat{ID: 6, Type: "private"}, Text: "x"})
	if in.UserName != "bob" {
		t.Errorf("username fallback = %q", in.UserName)
	}
}

type fakeTelegram struct {
	t       *testing.T
	mu      sync.Mutex
	updates []string // JSON of updates to deliver once
	sent    []map[string]any
	actions int
	flood   bool // first sendMessage answers 429
}

func (f *fakeTelegram) handler(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/bot"+token+"/") {
		f.t.Errorf("unexpected path %s", r.URL.Path)
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var body map[string]any
	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &body)

	f.mu.Lock()
	defer f.mu.Unlock()
	switch method {
	case "getMe":
		io.WriteString(w, `{"ok":true,"result":{"id":99,"is_bot":true,"first_name":"Janny","username":"JannyBot"}}`)
	case "getUpdates":
		if len(f.updates) > 0 {
			u := f.updates[0]
			f.updates = f.updates[1:]
			io.WriteString(w, `{"ok":true,"result":[`+u+`]}`)
			return
		}
		f.mu.Unlock()
		select {
		case <-r.Context().Done():
		case <-time.After(50 * time.Millisecond):
		}
		f.mu.Lock()
		io.WriteString(w, `{"ok":true,"result":[]}`)
	case "sendChatAction":
		f.actions++
		io.WriteString(w, `{"ok":true,"result":true}`)
	case "sendMessage":
		if f.flood {
			f.flood = false
			w.WriteHeader(429)
			io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
			return
		}
		f.sent = append(f.sent, body)
		io.WriteString(w, `{"ok":true,"result":{}}`)
	default:
		f.t.Errorf("unexpected method %s", method)
	}
}

func TestRunEndToEnd(t *testing.T) {
	f := &fakeTelegram{t: t, updates: []string{
		`{"update_id":10,"message":{"message_id":7,"from":{"id":5,"first_name":"Ann"},"chat":{"id":-100,"type":"supergroup"},"text":"@JannyBot hi there"}}`,
		`{"update_id":11,"message":{"message_id":8,"from":{"id":6,"first_name":"Bob"},"chat":{"id":6,"type":"private"},"text":"` + strings.Repeat("x", 10) + `"}}`,
	}}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()

	c := New(Config{Token: token, APIBase: srv.URL, PollTimeout: time.Second}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var got []channel.Incoming
	var mu sync.Mutex
	go func() {
		done <- c.Run(ctx, func(ctx context.Context, in channel.Incoming) {
			_ = in.Responder.Typing(ctx)
			_ = in.Responder.Send(ctx, "reply to "+in.Text)
			mu.Lock()
			got = append(got, in)
			mu.Unlock()
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		n := len(f.sent)
		f.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 2 || f.actions < 2 {
		t.Fatalf("sent=%d actions=%d", len(f.sent), f.actions)
	}
	var group map[string]any
	for _, s := range f.sent {
		if s["chat_id"] == float64(-100) {
			group = s
		}
	}
	if group == nil || group["text"] != "reply to hi there" {
		t.Fatalf("group reply = %v", group)
	}
	if rp, ok := group["reply_parameters"].(map[string]any); !ok || rp["message_id"] != float64(7) {
		t.Errorf("group replies must quote the message: %v", group)
	}
	for _, s := range f.sent {
		if s["chat_id"] == float64(6) {
			if _, has := s["reply_parameters"]; has {
				t.Error("private replies must not quote")
			}
		}
	}
}

func TestSendSplitsAndRetriesFlood(t *testing.T) {
	f := &fakeTelegram{t: t, flood: true}
	srv := httptest.NewServer(http.HandlerFunc(f.handler))
	defer srv.Close()
	c := newChannel(srv.URL)
	r := &responder{c: c, chatID: 1, thread: 42}
	long := strings.Repeat("ก", maxMessageRunes+500)
	start := time.Now()
	if err := r.Send(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < time.Second {
		t.Error("should have waited retry_after")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) != 2 || f.sent[0]["message_thread_id"] != float64(42) {
		t.Errorf("sent = %v", f.sent)
	}
}

func TestBadTokenIsFatalAndNeverLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	}))
	defer srv.Close()
	c := New(Config{Token: token, APIBase: srv.URL}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := c.Run(context.Background(), func(context.Context, channel.Incoming) {})
	if err == nil || !strings.Contains(err.Error(), "rejected the bot token") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "SECRET") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestNetworkErrorsAreRedacted(t *testing.T) {
	c := New(Config{Token: token, APIBase: "http://127.0.0.1:1"}, nil)
	err := c.call(context.Background(), "getMe", struct{}{}, nil)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}
