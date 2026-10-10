package ntfy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

var bg = context.Background()

func init() { backoff = time.Millisecond }

type server struct {
	mu      sync.Mutex
	bodies  []map[string]any
	auths   []string
	ctypes  []string
	paths   []string
	status  []int // statuses to answer with, in turn; then 200
	replies []string
}

func (s *server) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	s.mu.Lock()
	s.bodies = append(s.bodies, m)
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.ctypes = append(s.ctypes, r.Header.Get("Content-Type"))
	s.paths = append(s.paths, r.Method+" "+r.URL.Path)
	code := 200
	if len(s.status) > 0 {
		code, s.status = s.status[0], s.status[1:]
	}
	reply := `{"id":"x"}`
	if code >= 400 {
		reply = `{"code":` + http.StatusText(code) + `,"error":"it did not work"}`
		reply = `{"error":"it did not work"}`
	}
	s.mu.Unlock()
	if code >= 300 && code < 400 {
		w.Header().Set("Location", "http://elsewhere.example/")
	}
	w.WriteHeader(code)
	io.WriteString(w, reply)
}

func start(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	s := &server{}
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	return s, srv
}

func TestPublishSendsJSON(t *testing.T) {
	s, srv := start(t)
	c := &Client{BaseURL: srv.URL, TopicPrefix: "jannyq-", HTTP: srv.Client()}
	err := c.Publish(bg, Message{Topic: "peter-phone", Title: "call Peter", Body: "โทรหาคุณ Peter เรื่องใบเสนอราคา", Priority: 4, Tags: []string{"alarm_clock"}})
	if err != nil {
		t.Fatal(err)
	}
	b := s.bodies[0]
	tags, _ := b["tags"].([]any)
	if b["topic"] != "jannyq-peter-phone" || b["message"] != "โทรหาคุณ Peter เรื่องใบเสนอราคา" || b["title"] != "call Peter" ||
		b["priority"] != float64(4) || len(tags) != 1 || tags[0] != "alarm_clock" {
		t.Errorf("body = %v", b)
	}
	if s.ctypes[0] != "application/json" || s.paths[0] != "POST /" || s.auths[0] != "" {
		t.Errorf("%v %v %v", s.ctypes, s.paths, s.auths)
	}
	// the default priority and an empty title are left out
	_ = c.Publish(bg, Message{Topic: "x", Body: "hello", Priority: 3})
	if _, has := s.bodies[1]["priority"]; has {
		t.Errorf("default priority is sent: %v", s.bodies[1])
	}
	if _, has := s.bodies[1]["title"]; has {
		t.Errorf("empty title is sent: %v", s.bodies[1])
	}
	// a trailing slash on the address is fine
	c.BaseURL += "/"
	if err := c.Publish(bg, Message{Topic: "x", Body: "again"}); err != nil {
		t.Fatal(err)
	}
}

func TestAuthentication(t *testing.T) {
	s, srv := start(t)
	for token, want := range map[string]string{
		"tk_abc123":  "Bearer tk_abc123",
		"bob:secret": "Basic Ym9iOnNlY3JldA==",
	} {
		c := &Client{BaseURL: srv.URL, Token: token, HTTP: srv.Client()}
		if err := c.Publish(bg, Message{Topic: "t", Body: "x"}); err != nil {
			t.Fatal(err)
		}
		if got := s.auths[len(s.auths)-1]; got != want {
			t.Errorf("token %q: Authorization = %q, want %q", token, got, want)
		}
	}
}

func TestTopicRules(t *testing.T) {
	c := &Client{TopicPrefix: "jannyq-"}
	for in, want := range map[string]string{
		"phone":        "jannyq-phone",
		"jannyq-phone": "jannyq-phone", // not prefixed twice
		" My_Topic-1 ": "jannyq-My_Topic-1",
	} {
		if got, err := c.Topic(in); err != nil || got != want {
			t.Errorf("Topic(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "  ", "has space", "slash/topic", "dot.topic", "ไทย", "a?b", "../x", strings.Repeat("x", 60)} {
		if _, err := c.Topic(bad); err == nil {
			t.Errorf("Topic(%q) should be refused", bad)
		}
	}
	if got, err := (&Client{}).Topic("plain"); err != nil || got != "plain" {
		t.Errorf("no prefix: %q %v", got, err)
	}
	if got, err := (&Client{}).Topic(strings.Repeat("y", 64)); err != nil || len(got) != 64 {
		t.Errorf("64 characters are allowed: %v", err)
	}
	if _, err := (&Client{}).Topic(strings.Repeat("y", 65)); err == nil {
		t.Error("65 characters are not")
	}
}

func TestLongMessagesAreCutAtACharacter(t *testing.T) {
	s, srv := start(t)
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	thai := strings.Repeat("ก", 3000) // 9000 bytes
	if err := c.Publish(bg, Message{Topic: "t", Body: thai, Title: strings.Repeat("ข", 500)}); err != nil {
		t.Fatal(err)
	}
	msg := s.bodies[0]["message"].(string)
	title := s.bodies[0]["title"].(string)
	if len(msg) > 3900 || !utf8.ValidString(msg) || !strings.HasSuffix(msg, "…") || !strings.HasPrefix(msg, "กกก") {
		t.Errorf("message: %d bytes, valid=%v", len(msg), utf8.ValidString(msg))
	}
	if len(title) > 200 || !utf8.ValidString(title) {
		t.Errorf("title: %d bytes", len(title))
	}
	short := "short text"
	_ = c.Publish(bg, Message{Topic: "t", Body: short})
	if s.bodies[1]["message"] != short {
		t.Error("a short message must be sent as it is")
	}
}

func TestFailures(t *testing.T) {
	s, srv := start(t)
	c := &Client{BaseURL: srv.URL, Token: "tk_SECRET", HTTP: srv.Client()}

	// a refusal is not retried, and says what to check
	s.status = []int{403}
	err := c.Publish(bg, Message{Topic: "t", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "token") || len(s.bodies) != 1 {
		t.Errorf("403: %v after %d requests", err, len(s.bodies))
	}
	// a server error is tried again, and may succeed
	s.bodies = nil
	s.status = []int{500, 502}
	if err := c.Publish(bg, Message{Topic: "t", Body: "x"}); err != nil || len(s.bodies) != 3 {
		t.Errorf("5xx then ok: %v after %d requests", err, len(s.bodies))
	}
	// a bad request is final and carries the server's explanation
	s.bodies = nil
	s.status = []int{400}
	if err := c.Publish(bg, Message{Topic: "t", Body: "x"}); err == nil || !strings.Contains(err.Error(), "it did not work") || len(s.bodies) != 1 {
		t.Errorf("400: %v after %d requests", err, len(s.bodies))
	}
	// redirects are refused, so that credentials go nowhere else
	s.bodies = nil
	s.status = []int{302}
	if err := c.Publish(bg, Message{Topic: "t", Body: "x"}); err == nil || !strings.Contains(err.Error(), "redirect") || len(s.bodies) != 1 {
		t.Errorf("302: %v", err)
	}
	// nothing leaks the token
	for _, code := range []int{401, 403, 400, 500} {
		s.status = []int{code, code, code}
		if err := c.Publish(bg, Message{Topic: "t", Body: "x"}); err != nil && strings.Contains(err.Error(), "SECRET") {
			t.Errorf("the error for %d leaks the token: %v", code, err)
		}
	}
	// an unreachable server: no address, no token in the message
	dead := &Client{BaseURL: "http://127.0.0.1:1", Token: "tk_SECRET", HTTP: &http.Client{Timeout: time.Second}}
	err = dead.Publish(bg, Message{Topic: "t", Body: "x"})
	if err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "127.0.0.1") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("unreachable: %v", err)
	}
	// input errors need no request
	if err := c.Publish(bg, Message{Topic: "bad topic", Body: "x"}); err == nil {
		t.Error("a bad topic")
	}
	if err := c.Publish(bg, Message{Topic: "t", Body: "  "}); err == nil {
		t.Error("an empty message")
	}
	var none *Client
	if err := none.Publish(bg, Message{Topic: "t", Body: "x"}); err != ErrDisabled {
		t.Errorf("no client: %v", err)
	}
	if err := (&Client{}).Publish(bg, Message{Topic: "t", Body: "x"}); err != ErrDisabled {
		t.Errorf("no address: %v", err)
	}
	// a cancelled context stops the retries
	ctx, cancel := context.WithCancel(bg)
	cancel()
	s.status = []int{500, 500, 500}
	if err := c.Publish(ctx, Message{Topic: "t", Body: "x"}); err == nil {
		t.Error("a cancelled context")
	}
}

func TestShortDoesNotRevealTheTopic(t *testing.T) {
	a, b := Short("my-secret-topic"), Short("another-topic")
	if a == b || strings.Contains(a, "secret") || len(a) != 6 || Short("my-secret-topic") != a {
		t.Errorf("%q %q", a, b)
	}
}
