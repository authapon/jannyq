package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/server"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

type harness struct {
	t      *testing.T
	ts     *httptest.Server
	ch     *Channel
	client *http.Client
	cancel context.CancelFunc
	done   chan struct{}

	mu   sync.Mutex
	seen []channel.Incoming
	// reply, when set, answers each incoming message (in its own goroutine).
	reply func(in channel.Incoming)
}

type fakeHistory struct {
	turns map[string][]Turn
}

func (f fakeHistory) Recent(_ context.Context, id string, n int) ([]Turn, error) {
	t := f.turns[id]
	if len(t) > n {
		t = t[len(t)-n:]
	}
	return t, nil
}

func newHarness(t *testing.T, tweak func(*Config), trusted ...string) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	var proxies []netip.Prefix
	if len(trusted) > 0 {
		var err error
		if proxies, err = server.ParseProxies(trusted); err != nil {
			t.Fatal(err)
		}
	}
	srv := server.New(server.Options{Addr: ":0", TrustedProxies: proxies, Log: log})
	cfg := Config{
		Title: "Test bot", Lang: "en", Secret: secret, MaxMessage: 100, IPRate: 1000, NewSessionsPerHour: 1000,
		SecureCookies: "off", Strings: map[string]string{"web_send": "Send"},
	}
	if tweak != nil {
		tweak(&cfg)
	}
	ch, err := New(cfg, srv, log)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, ch: ch, done: make(chan struct{})}
	h.ts = httptest.NewServer(srv.Handler())
	jar, _ := cookiejar.New(nil)
	h.client = &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		defer close(h.done)
		_ = ch.Run(ctx, func(ctx context.Context, in channel.Incoming) {
			h.mu.Lock()
			h.seen = append(h.seen, in)
			reply := h.reply
			h.mu.Unlock()
			if reply != nil {
				reply(in)
			}
		})
	}()
	waitUntil(t, func() bool { ch.mu.Lock(); defer ch.mu.Unlock(); return ch.sink != nil })
	t.Cleanup(func() { h.stop() })
	return h
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Error("channel did not stop")
	}
	h.ts.Close()
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func (h *harness) incoming() []channel.Incoming {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]channel.Incoming(nil), h.seen...)
}

func (h *harness) do(method, path, body string, hdr map[string]string) *http.Response {
	h.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, h.ts.URL+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) json(method, path, body string, hdr map[string]string) (int, map[string]any) {
	h.t.Helper()
	resp := h.do(method, path, body, hdr)
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// start makes the harness client visit the page so that it gets a session.
func (h *harness) start() {
	h.t.Helper()
	if code, m := h.json("GET", "/api/config", "", nil); code != 200 || m["authed"] != true {
		h.t.Fatalf("config: %d %v", code, m)
	}
}

func (h *harness) cookieValue() string {
	for _, c := range h.client.Jar.Cookies(mustURL(h.ts.URL)) {
		if c.Name == cookieName {
			return c.Value
		}
	}
	return ""
}

// --- server-sent events client ---

type sseEvent struct{ ID, Type, Data string }

type stream struct {
	resp   *http.Response
	events chan sseEvent
}

func (h *harness) stream(hdr map[string]string) *stream {
	h.t.Helper()
	resp := h.do("GET", "/api/events", "", hdr)
	if resp.StatusCode != 200 {
		h.t.Fatalf("events: %d", resp.StatusCode)
	}
	s := &stream{resp: resp, events: make(chan sseEvent, 100)}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(resp.Body)
		var ev sseEvent
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.Type != "" {
					s.events <- ev
				}
				ev = sseEvent{}
			case strings.HasPrefix(line, "id: "):
				ev.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.Type = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.Data = line[6:]
			}
		}
	}()
	h.t.Cleanup(func() { resp.Body.Close() })
	return s
}

func (s *stream) next(t *testing.T) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatal("stream closed")
		}
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event in time")
	}
	return sseEvent{}
}

func (s *stream) expectNone(t *testing.T) {
	t.Helper()
	select {
	case ev := <-s.events:
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(150 * time.Millisecond):
	}
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// --- tests ---

func TestConfigIssuesASignedSessionCookie(t *testing.T) {
	h := newHarness(t, nil)
	resp := h.do("GET", "/api/config", "", nil)
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	if m["title"] != "Test bot" || m["authed"] != true || m["needsCode"] != false || m["maxMessage"] != float64(100) {
		t.Errorf("config = %v", m)
	}
	if m["strings"].(map[string]any)["web_send"] != "Send" {
		t.Errorf("strings = %v", m["strings"])
	}
	var ck *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			ck = c
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode || ck.Path != "/" || ck.Secure || ck.MaxAge <= 0 {
		t.Fatalf("cookie = %+v", ck)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("config must not be cached")
	}
	// a returning visitor keeps the same session
	first := h.cookieValue()
	h.start()
	if h.cookieValue() != first {
		t.Error("the session changed on a second visit")
	}
}

func TestCookieIsSecureBehindAnHTTPSProxy(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.SecureCookies = "auto" }, "127.0.0.0/8")
	resp := h.do("GET", "/api/config", "", map[string]string{"X-Forwarded-Proto": "https"})
	resp.Body.Close()
	secure := false
	for _, c := range resp.Cookies() {
		secure = secure || (c.Name == cookieName && c.Secure)
	}
	if !secure {
		t.Error("cookie must be Secure when the client used HTTPS")
	}
	if resp.Header.Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing behind an HTTPS proxy")
	}

	h2 := newHarness(t, func(c *Config) { c.SecureCookies = "auto" }) // proxies not trusted
	resp = h2.do("GET", "/api/config", "", map[string]string{"X-Forwarded-Proto": "https"})
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Secure {
			t.Error("a forged X-Forwarded-Proto from an untrusted peer was believed")
		}
	}
	h3 := newHarness(t, func(c *Config) { c.SecureCookies = "on" })
	resp = h3.do("GET", "/api/config", "", nil)
	resp.Body.Close()
	if len(resp.Cookies()) == 0 || !resp.Cookies()[0].Secure {
		t.Error("secure-cookies=on must always set Secure")
	}
}

func TestSendDeliversToTheSinkAndRepliesComeBackOverSSE(t *testing.T) {
	h := newHarness(t, nil)
	h.reply = func(in channel.Incoming) {
		_ = in.Responder.Typing(context.Background())
		_ = in.Responder.Send(context.Background(), "echo: "+in.Text)
	}
	h.start()
	s := h.stream(nil)

	code, m := h.json("POST", "/api/send", `{"text":"  hello <b>world</b>  "}`, nil)
	if code != 202 || m["accepted"] != true {
		t.Fatalf("send: %d %v", code, m)
	}
	if ev := s.next(t); ev.Type != "typing" || ev.ID != "" {
		t.Errorf("first event = %+v", ev)
	}
	ev := s.next(t)
	var payload struct{ Text string }
	_ = json.Unmarshal([]byte(ev.Data), &payload)
	if ev.Type != "message" || ev.ID != "1" || payload.Text != "echo: hello <b>world</b>" {
		t.Errorf("reply event = %+v", ev)
	}
	// markup in the reply is escaped on the wire, so it can never be read as HTML
	if strings.Contains(ev.Data, "<b>") {
		t.Errorf("raw angle brackets in the event data: %s", ev.Data)
	}

	got := h.incoming()
	if len(got) != 1 {
		t.Fatalf("sink got %d messages", len(got))
	}
	in := got[0]
	id := strings.SplitN(h.cookieValue(), ".", 2)[0]
	if in.Channel != "web" || in.ChatID != id || in.UserID != id || in.Text != "hello <b>world</b>" || in.IsGroup || !in.Addressed {
		t.Errorf("incoming = %+v", in)
	}
}

func TestEachVisitorGetsAnIsolatedChat(t *testing.T) {
	h := newHarness(t, nil)
	h.reply = func(in channel.Incoming) { _ = in.Responder.Send(context.Background(), "for "+in.ChatID) }
	h.start()
	a := h.stream(nil)

	// a second browser
	other := &harness{t: t, ts: h.ts}
	jar, _ := cookiejar.New(nil)
	other.client = &http.Client{Jar: jar}
	other.start()
	b := other.stream(nil)

	h.json("POST", "/api/send", `{"text":"from a"}`, nil)
	other.json("POST", "/api/send", `{"text":"from b"}`, nil)
	idA := strings.SplitN(h.cookieValue(), ".", 2)[0]
	idB := strings.SplitN(other.cookieValue(), ".", 2)[0]
	if idA == idB {
		t.Fatal("visitors share a session")
	}
	if ev := a.next(t); !strings.Contains(ev.Data, idA) {
		t.Errorf("a got %+v", ev)
	}
	if ev := b.next(t); !strings.Contains(ev.Data, idB) {
		t.Errorf("b got %+v", ev)
	}
	a.expectNone(t)
	b.expectNone(t)
}

func TestSSEResumesWithLastEventID(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	id := strings.SplitN(h.cookieValue(), ".", 2)[0]
	first := h.stream(nil)
	h.ch.hub.publish(id, "message", map[string]string{"text": "one"})
	if ev := first.next(t); ev.ID != "1" {
		t.Fatalf("first = %+v", ev)
	}
	first.resp.Body.Close() // the connection drops...

	waitUntil(t, func() bool { return h.ch.hub.openStreams() == 0 })
	h.ch.hub.publish(id, "message", map[string]string{"text": "two"}) // ...and replies arrive meanwhile
	h.ch.hub.publish(id, "message", map[string]string{"text": "three"})

	again := h.stream(map[string]string{"Last-Event-ID": "1"})
	if ev := again.next(t); ev.ID != "2" || !strings.Contains(ev.Data, "two") {
		t.Errorf("replayed = %+v", ev)
	}
	if ev := again.next(t); ev.ID != "3" {
		t.Errorf("replayed = %+v", ev)
	}
	fresh := h.stream(nil) // a new page load shows history from the database instead
	fresh.expectNone(t)
}

func TestAuthenticationIsRequired(t *testing.T) {
	h := newHarness(t, nil) // no session yet
	for _, tc := range []struct{ method, path, body string }{
		{"POST", "/api/send", `{"text":"hi"}`},
		{"GET", "/api/events", ""},
		{"GET", "/api/history", ""},
	} {
		if code, _ := h.json(tc.method, tc.path, tc.body, nil); code != 401 {
			t.Errorf("%s %s without a session: %d", tc.method, tc.path, code)
		}
	}
	// a forged or tampered cookie is no session either
	for name, value := range map[string]string{
		"forged":   strings.Repeat("a", 32) + ".AAAA",
		"tampered": "",
	} {
		if name == "tampered" {
			h.start()
			v := h.cookieValue()
			value = "b" + v[1:]
		}
		code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, map[string]string{"Cookie": cookieName + "=" + value})
		if code != 401 && name == "forged" {
			t.Errorf("%s cookie accepted: %d", name, code)
		}
	}
	if len(h.incoming()) != 0 {
		t.Error("a message without a valid session reached the bot")
	}
}

func TestAccessCode(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AccessCode = "open-sesame" })
	code, m := h.json("GET", "/api/config", "", nil)
	if code != 200 || m["needsCode"] != true || m["authed"] != false {
		t.Fatalf("config = %v", m)
	}
	if h.cookieValue() != "" {
		t.Error("a session was issued without the access code")
	}
	if code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, nil); code != 401 {
		t.Errorf("send without login: %d", code)
	}
	if code, _ := h.json("POST", "/api/login", `{"code":"wrong"}`, nil); code != 403 {
		t.Errorf("wrong code: %d", code)
	}
	if h.cookieValue() != "" {
		t.Error("a wrong code produced a session")
	}
	if code, _ := h.json("POST", "/api/login", `{"code":"open-sesame"}`, nil); code != 200 {
		t.Fatalf("right code refused: %d", code)
	}
	if _, m := h.json("GET", "/api/config", "", nil); m["authed"] != true {
		t.Errorf("not authed after login: %v", m)
	}
	if code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, nil); code != 202 {
		t.Errorf("send after login: %d", code)
	}
}

func TestAccessCodeBruteForceIsLimitedAndCookiesDieWithTheCode(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AccessCode = "open-sesame" })
	limited := 0
	for i := 0; i < 15; i++ {
		if code, _ := h.json("POST", "/api/login", `{"code":"guess"}`, nil); code == 429 {
			limited++
		}
	}
	if limited < 4 {
		t.Errorf("only %d of 15 guesses were throttled", limited)
	}
	if code, _ := h.json("POST", "/api/login", `{"code":"open-sesame"}`, nil); code != 429 {
		t.Errorf("even the right code must wait while throttled: %d", code)
	}

	// an old session stops working when the code is changed
	a := newHarness(t, func(c *Config) { c.AccessCode = "code-one" })
	a.json("POST", "/api/login", `{"code":"code-one"}`, nil)
	old := a.cookieValue()
	b := newHarness(t, func(c *Config) { c.AccessCode = "code-two" })
	code, _ := b.json("POST", "/api/send", `{"text":"hi"}`, map[string]string{"Cookie": cookieName + "=" + old})
	if code != 401 {
		t.Errorf("a cookie from before the code change was accepted: %d", code)
	}
	// login is not available when no code is configured
	open := newHarness(t, nil)
	if code, _ := open.json("POST", "/api/login", `{"code":"x"}`, nil); code != 404 {
		t.Errorf("login without a configured code: %d", code)
	}
}

func TestCrossSiteRequestsAreRefused(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.AllowedOrigins = []string{"https://trusted.example"} })
	h.start()
	host := strings.TrimPrefix(h.ts.URL, "http://")
	send := func(hdr map[string]string) int {
		code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, hdr)
		return code
	}
	for name, hdr := range map[string]map[string]string{
		"foreign origin":   {"Origin": "https://evil.example"},
		"cross-site fetch": {"Sec-Fetch-Site": "cross-site"},
		"same-site fetch":  {"Sec-Fetch-Site": "same-site"},
		"malformed origin": {"Origin": "://"},
		"null origin":      {"Origin": "null"},
	} {
		if code := send(hdr); code != 403 {
			t.Errorf("%s: %d, want 403", name, code)
		}
	}
	for name, hdr := range map[string]map[string]string{
		"same origin":           {"Origin": "http://" + host},
		"same-origin fetch":     {"Sec-Fetch-Site": "same-origin", "Origin": "http://" + host},
		"typed in the address":  {"Sec-Fetch-Site": "none"},
		"no browser headers":    nil,
		"explicitly allowed":    {"Origin": "https://trusted.example"},
		"allowed with trailing": {"Origin": "https://trusted.example"},
	} {
		if code := send(hdr); code != 202 {
			t.Errorf("%s: %d, want 202", name, code)
		}
	}
	// a form post (text/plain, the only thing cross-site forms can send) is refused outright
	resp := h.do("POST", "/api/send", `{"text":"hi"}`, map[string]string{"Content-Type": "text/plain"})
	resp.Body.Close()
	if resp.StatusCode != 415 {
		t.Errorf("text/plain: %d", resp.StatusCode)
	}
	if code, _ := h.json("POST", "/api/login", `{"code":"x"}`, map[string]string{"Origin": "https://evil.example"}); code != 403 && code != 404 {
		t.Errorf("login from another origin: %d", code)
	}
}

func TestMessageValidation(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	for name, tc := range map[string]struct {
		body string
		want int
	}{
		"ok":             {`{"text":"hello"}`, 202},
		"thai at limit":  {`{"text":"` + strings.Repeat("ก", 100) + `"}`, 202},
		"one over limit": {`{"text":"` + strings.Repeat("ก", 101) + `"}`, 413},
		"empty":          {`{"text":""}`, 400},
		"blank":          {`{"text":"  \n\t "}`, 400},
		"no text":        {`{}`, 400},
		"not json":       {`hello`, 400},
		"wrong type":     {`{"text":42}`, 400},
		"huge body":      {`{"text":"` + strings.Repeat("a", maxBodyBytes) + `"}`, 413},
	} {
		if code, _ := h.json("POST", "/api/send", tc.body, nil); code != tc.want {
			t.Errorf("%s: %d, want %d", name, code, tc.want)
		}
	}
	if n := len(h.incoming()); n != 2 {
		t.Errorf("%d messages reached the bot, want 2", n)
	}
}

func TestPerIPMessageRateLimit(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.IPRate = 3 })
	h.start()
	var codes []int
	for i := 0; i < 5; i++ {
		code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, nil)
		codes = append(codes, code)
	}
	if codes[2] != 202 || codes[3] != 429 || codes[4] != 429 {
		t.Errorf("codes = %v", codes)
	}
	// dropping the cookie does not reset the limit: it is per address
	jar, _ := cookiejar.New(nil)
	fresh := &harness{t: t, ts: h.ts, client: &http.Client{Jar: jar}}
	fresh.start()
	if code, _ := fresh.json("POST", "/api/send", `{"text":"hi"}`, nil); code != 429 {
		t.Errorf("a new session from the same address skipped the limit: %d", code)
	}
}

func TestNewSessionsPerIPAreLimited(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.NewSessionsPerHour = 2 })
	var codes []int
	var first *harness
	for i := 0; i < 4; i++ {
		jar, _ := cookiejar.New(nil)
		c := &harness{t: t, ts: h.ts, client: &http.Client{Jar: jar}}
		if first == nil {
			first = c
		}
		code, _ := c.json("GET", "/api/config", "", nil)
		codes = append(codes, code)
	}
	if codes[0] != 200 || codes[1] != 200 || codes[2] != 429 || codes[3] != 429 {
		t.Errorf("codes = %v", codes)
	}
	// a returning visitor already has a session and is not counted again
	if code, m := first.json("GET", "/api/config", "", nil); code != 200 || m["authed"] != true {
		t.Errorf("returning visitor: %d %v", code, m)
	}
}

func TestInFlightCap(t *testing.T) {
	gate := make(chan struct{})
	h := newHarness(t, func(c *Config) { c.MaxInFlight = 2 })
	h.reply = func(channel.Incoming) { <-gate }
	defer close(gate)
	h.start()
	for i := 0; i < 2; i++ {
		if code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, nil); code != 202 {
			t.Fatalf("message %d: %d", i, code)
		}
	}
	if code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, nil); code != 503 {
		t.Errorf("third message while two are running: %d", code)
	}
}

func TestStreamLimits(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	for i := 0; i < maxSubsPerSession; i++ {
		h.stream(nil)
	}
	resp := h.do("GET", "/api/events", "", nil)
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Errorf("fifth stream of one chat: %d", resp.StatusCode)
	}
}

func TestHistory(t *testing.T) {
	var hist fakeHistory
	h := newHarness(t, func(c *Config) { c.History = &hist })
	h.start()
	id := strings.SplitN(h.cookieValue(), ".", 2)[0]
	hist.turns = map[string][]Turn{
		id:        {{"user", "hi"}, {"assistant", "hello"}},
		"someone": {{"user", "secret"}},
	}
	code, m := h.json("GET", "/api/history", "", nil)
	msgs, _ := m["messages"].([]any)
	if code != 200 || len(msgs) != 2 || msgs[1].(map[string]any)["text"] != "hello" {
		t.Errorf("history = %d %v", code, m)
	}
	hist.turns = nil
	if _, m := h.json("GET", "/api/history", "", nil); m["messages"] == nil {
		t.Error("an empty history must be [], not null")
	}
}

func TestPagesAreServedWithAStrictCSP(t *testing.T) {
	h := newHarness(t, nil)
	for path, ctype := range map[string]string{
		"/":        "text/html",
		"/app.js":  "text/javascript",
		"/app.css": "text/css",
	} {
		resp := h.do("GET", path, "", nil)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), ctype) || len(body) < 100 {
			t.Errorf("%s: %d %q (%d bytes)", path, resp.StatusCode, resp.Header.Get("Content-Type"), len(body))
		}
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q: %s", path, want, csp)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP allows inline code: %s", path, csp)
		}
		if resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing framing/sniffing headers", path)
		}
	}
	index := func() string {
		resp := h.do("GET", "/", "", nil)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}()
	if strings.Contains(index, "<script>") || strings.Contains(index, "onclick=") {
		t.Error("the page uses inline scripts, which the CSP forbids")
	}
}

func TestCustomBasePath(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.BasePath = "chat" })
	for path, want := range map[string]int{"/chat/": 200, "/chat/app.js": 200, "/chat/api/config": 200, "/": 404, "/api/config": 404} {
		resp := h.do("GET", path, "", nil)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}
	resp := h.do("GET", "/chat", "", nil)
	resp.Body.Close()
	if resp.StatusCode != 301 || resp.Header.Get("Location") != "/chat/" {
		t.Errorf("/chat → %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, err := http.Get(h.ts.URL + "/chat/api/config") // a visitor without a session cookie
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	path := ""
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			path = c.Path
		}
	}
	if path != "/chat/" {
		t.Errorf("cookie path = %q, want it scoped to the chat", path)
	}
}

func TestShutdownEndsOpenStreams(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	s := h.stream(nil)
	h.cancel()
	select {
	case _, ok := <-s.events:
		if ok {
			t.Log("event before close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stayed open after shutdown")
	}
	<-h.done
}

func TestSendBeforeRunIsRefused(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", Log: log})
	ch, err := New(Config{Secret: secret, SecureCookies: "off", IPRate: 100, NewSessionsPerHour: 100}, srv, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	jar, _ := cookiejar.New(nil)
	h := &harness{t: t, ts: ts, ch: ch, client: &http.Client{Jar: jar}}
	h.start()
	if code, _ := h.json("POST", "/api/send", `{"text":"hi"}`, nil); code != 503 {
		t.Errorf("code = %d", code)
	}
}

func TestNewNeedsAStrongSecret(t *testing.T) {
	srv := server.New(server.Options{Addr: ":0"})
	if _, err := New(Config{Secret: []byte("short")}, srv, nil); err == nil {
		t.Error("a short secret was accepted")
	}
}

func TestMessagesFromOneVisitorAreAcceptedInTheOrderTheyWereSent(t *testing.T) {
	h := newHarness(t, nil)
	var mu sync.Mutex
	var order []string
	h.reply = nil
	// replace the harness sink behaviour: uneven delay before accepting
	h.ch.mu.Lock()
	h.ch.sink = func(ctx context.Context, in channel.Incoming) {
		n, _ := strconv.Atoi(strings.TrimPrefix(in.Text, "m"))
		time.Sleep(time.Duration((n*13)%7) * time.Millisecond)
		mu.Lock()
		order = append(order, in.Text)
		mu.Unlock()
		in.Accepted()
		time.Sleep(3 * time.Millisecond)
	}
	h.ch.mu.Unlock()
	h.start()
	for i := 1; i <= 25; i++ {
		if code, _ := h.json("POST", "/api/send", fmt.Sprintf(`{"text":"m%d"}`, i), nil); code != 202 {
			t.Fatalf("send %d: %d", i, code)
		}
	}
	waitUntil(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(order) == 25 })
	mu.Lock()
	defer mu.Unlock()
	for i, got := range order {
		if got != fmt.Sprintf("m%d", i+1) {
			t.Fatalf("accepted order = %v", order)
		}
	}
}
