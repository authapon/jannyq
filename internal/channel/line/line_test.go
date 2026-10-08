package line

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/server"
)

const (
	secret = "channel-secret-123"
	token  = "ACCESS.TOKEN.SECRET"
)

type apiCall struct {
	Path string
	Body map[string]any
}

type fakeLine struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	calls   []apiCall
	profile map[string]string // path -> display name
	content map[string][]byte // message id -> bytes
	replyOK bool
}

func newFakeLine(t *testing.T) *fakeLine {
	f := &fakeLine{t: t, profile: map[string]string{}, content: map[string][]byte{}, replyOK: true}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLine) handle(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+token {
		f.t.Errorf("authorization = %q on %s", r.Header.Get("Authorization"), r.URL.Path)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/content"):
		id := strings.Split(r.URL.Path, "/")[4]
		b, ok := f.content[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case r.Method == "GET":
		name, ok := f.profile[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			io.WriteString(w, `{"message":"Not found"}`)
			return
		}
		fmt.Fprintf(w, `{"displayName":%q}`, name)
	default:
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.calls = append(f.calls, apiCall{r.URL.Path, body})
		if r.URL.Path == "/v2/bot/message/reply" && !f.replyOK {
			w.WriteHeader(400)
			io.WriteString(w, `{"message":"Invalid reply token"}`)
			return
		}
		io.WriteString(w, `{}`)
	}
}

func (f *fakeLine) sent() []apiCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]apiCall(nil), f.calls...)
}

func (f *fakeLine) only(path string) []apiCall {
	var out []apiCall
	for _, c := range f.sent() {
		if c.Path == path {
			out = append(out, c)
		}
	}
	return out
}

type rig struct {
	t      *testing.T
	api    *fakeLine
	srv    *server.Server
	web    *httptest.Server
	ch     *Channel
	in     chan channel.Incoming
	cancel context.CancelFunc
	done   chan error
	now    time.Time
	mu     sync.Mutex
}

func newRig(t *testing.T, handle func(channel.Incoming)) *rig {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := newFakeLine(t)
	srv := server.New(server.Options{Addr: ":0", RatePerMinute: 2, Log: log})
	ch, err := New(Config{ChannelSecret: secret, ChannelToken: token, APIBase: api.srv.URL, DataAPIBase: api.srv.URL}, srv, log)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, api: api, srv: srv, ch: ch, in: make(chan channel.Incoming, 100), done: make(chan error, 1), now: time.Now()}
	ch.now = func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now }
	r.web = httptest.NewServer(srv.Handler())
	t.Cleanup(r.web.Close)
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go func() {
		r.done <- ch.Run(ctx, func(ctx context.Context, in channel.Incoming) {
			if handle != nil {
				handle(in)
			}
			in.Accepted() // after the hook: the next message of the chat may run as soon as this is called
			r.in <- in
		})
	}()
	for i := 0; i < 200; i++ { // wait until Run has installed the sink
		ch.mu.Lock()
		ok := ch.sink != nil
		ch.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return r
}

func (r *rig) advance(d time.Duration) { r.mu.Lock(); r.now = r.now.Add(d); r.mu.Unlock() }

func sign(body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

func (r *rig) post(body string, headers map[string]string) int {
	r.t.Helper()
	req, _ := http.NewRequest("POST", r.web.URL+"/webhook/line", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Line-Signature", sign([]byte(body)))
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (r *rig) send(events ...map[string]any) int {
	r.t.Helper()
	b, _ := json.Marshal(map[string]any{"destination": "Ubot", "events": events})
	return r.post(string(b), nil)
}

func (r *rig) next() channel.Incoming {
	r.t.Helper()
	select {
	case in := <-r.in:
		return in
	case <-time.After(5 * time.Second):
		r.t.Fatal("nothing reached the sink")
	}
	return channel.Incoming{}
}

func (r *rig) none() {
	r.t.Helper()
	select {
	case in := <-r.in:
		r.t.Fatalf("unexpected message: %+v", in)
	case <-time.After(200 * time.Millisecond):
	}
}

var evSeq int

func textEvent(source map[string]any, text string, extra map[string]any) map[string]any {
	evSeq++
	msg := map[string]any{"id": fmt.Sprint(1000 + evSeq), "type": "text", "text": text}
	for k, v := range extra {
		msg[k] = v
	}
	return map[string]any{"type": "message", "mode": "active", "timestamp": 1700000000123, "webhookEventId": fmt.Sprintf("ev%d", evSeq),
		"replyToken": fmt.Sprintf("rt%d", evSeq), "source": source, "message": msg}
}

var (
	userSrc  = map[string]any{"type": "user", "userId": "U1"}
	groupSrc = map[string]any{"type": "group", "groupId": "G1", "userId": "U1"}
)

func TestSignatureIsChecked(t *testing.T) {
	r := newRig(t, nil)
	body := `{"events":[]}`
	if code := r.post(body, nil); code != 200 {
		t.Errorf("a signed verification request: %d", code)
	}
	if code := r.post(body, map[string]string{"X-Line-Signature": ""}); code != 401 {
		t.Errorf("no signature: %d", code)
	}
	if code := r.post(body, map[string]string{"X-Line-Signature": "AAAA"}); code != 401 {
		t.Errorf("a wrong signature: %d", code)
	}
	// a body that was changed after signing
	b, _ := json.Marshal(map[string]any{"events": []any{textEvent(userSrc, "real", nil)}})
	forged := strings.Replace(string(b), "real", "fake", 1)
	if code := r.post(forged, map[string]string{"X-Line-Signature": sign(b)}); code != 401 {
		t.Errorf("a tampered body: %d", code)
	}
	r.none()
	// the right secret, for another bot
	other := hmac.New(sha256.New, []byte("another-secret"))
	other.Write(b)
	if code := r.post(string(b), map[string]string{"X-Line-Signature": base64.StdEncoding.EncodeToString(other.Sum(nil))}); code != 401 {
		t.Errorf("another secret: %d", code)
	}
	r.none()
	// oversized
	big := `{"events":[],"pad":"` + strings.Repeat("x", 2<<20) + `"}`
	if code := r.post(big, nil); code != 413 {
		t.Errorf("oversized: %d", code)
	}
	// garbage with a valid signature
	if code := r.post("not json", nil); code != 400 {
		t.Errorf("not json: %d", code)
	}
}

func TestRefusedRequestsAreLimitedPerAddress(t *testing.T) {
	r := newRig(t, nil)
	var last int
	for i := 0; i < badSignaturesPerMinute+5; i++ {
		last = r.post(`{"events":[]}`, map[string]string{"X-Line-Signature": "bad"})
	}
	if last != 429 {
		t.Errorf("an address that keeps failing is still served: %d", last)
	}
	// from the same address even a good request is turned away for now: that is the price of the flood
	if code := r.post(`{"events":[]}`, nil); code != 429 {
		t.Errorf("after the limit: %d", code)
	}
}

func TestTheWebhookIsNotLimitedLikeOtherRoutes(t *testing.T) {
	r := newRig(t, nil) // the server allows 2 requests per minute and address
	for i := 0; i < 10; i++ {
		if code := r.send(); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
}

func TestPrivateMessageAndReplies(t *testing.T) {
	r := newRig(t, func(in channel.Incoming) {
		_ = in.Responder.Typing(context.Background())
		_ = in.Responder.Send(context.Background(), "first answer")
		_ = in.Responder.Send(context.Background(), "second answer")
	})
	r.api.profile["/v2/bot/profile/U1"] = "Ann"
	if code := r.send(textEvent(userSrc, "hello bot", nil)); code != 200 {
		t.Fatal(code)
	}
	in := r.next()
	if in.Text != "hello bot" || !in.Addressed || in.IsGroup || in.ChatID != "U1" || in.UserID != "U1" || in.UserName != "Ann" || in.Channel != "line" {
		t.Errorf("incoming = %+v", in)
	}
	if !in.ReceivedAt.Equal(time.UnixMilli(1700000000123)) {
		t.Errorf("time = %v", in.ReceivedAt)
	}
	replies, pushes := r.api.only("/v2/bot/message/reply"), r.api.only("/v2/bot/message/push")
	if len(replies) != 1 || replies[0].Body["replyToken"] == nil || len(pushes) != 1 {
		t.Fatalf("calls = %+v", r.api.sent())
	}
	m := replies[0].Body["messages"].([]any)[0].(map[string]any)
	if m["type"] != "text" || m["text"] != "first answer" {
		t.Errorf("reply = %v", replies[0].Body)
	}
	if pushes[0].Body["to"] != "U1" {
		t.Errorf("push = %v", pushes[0].Body)
	}
	if n := len(r.api.only("/v2/bot/chat/loading/start")); n != 1 {
		t.Errorf("loading animations: %d", n)
	}
}

func TestReplyTokenFallbacks(t *testing.T) {
	r := newRig(t, nil)
	rs := &responder{c: r.ch, chatID: "U1", token: "tok", tokenAt: r.ch.now()}
	// an expired token is not even tried
	r.advance(2 * time.Minute)
	if err := rs.Send(context.Background(), "late"); err != nil {
		t.Fatal(err)
	}
	if len(r.api.only("/v2/bot/message/reply")) != 0 || len(r.api.only("/v2/bot/message/push")) != 1 {
		t.Errorf("calls = %+v", r.api.sent())
	}
	// a token LINE refuses: the text is pushed instead
	r.api.mu.Lock()
	r.api.replyOK = false
	r.api.calls = nil
	r.api.mu.Unlock()
	rs = &responder{c: r.ch, chatID: "G1", isGroup: true, token: "tok2", tokenAt: r.ch.now()}
	if err := rs.Send(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(r.api.only("/v2/bot/message/push")) != 1 {
		t.Errorf("calls = %+v", r.api.sent())
	}
	// a token works once
	r.api.mu.Lock()
	r.api.replyOK, r.api.calls = true, nil
	r.api.mu.Unlock()
	rs = &responder{c: r.ch, chatID: "U1", token: "tok3", tokenAt: r.ch.now()}
	_ = rs.Send(context.Background(), "one")
	_ = rs.Send(context.Background(), "two")
	if len(r.api.only("/v2/bot/message/reply")) != 1 || len(r.api.only("/v2/bot/message/push")) != 1 {
		t.Errorf("calls = %+v", r.api.sent())
	}
}

func TestLongTextIsSplitAndBatched(t *testing.T) {
	r := newRig(t, nil)
	rs := &responder{c: r.ch, chatID: "U1", token: "tok", tokenAt: r.ch.now()}
	text := strings.Repeat("ก", maxTextRunes*7+10)
	if err := rs.Send(context.Background(), text); err != nil {
		t.Fatal(err)
	}
	calls := r.api.sent()
	total := 0
	for i, c := range calls {
		msgs := c.Body["messages"].([]any)
		if len(msgs) > 5 {
			t.Errorf("call %d carries %d messages", i, len(msgs))
		}
		for _, m := range msgs {
			n := len([]rune(m.(map[string]any)["text"].(string)))
			if n > maxTextRunes {
				t.Errorf("a message of %d characters", n)
			}
			total += n
		}
	}
	if len(calls) != 2 || calls[0].Path != "/v2/bot/message/reply" || calls[1].Path != "/v2/bot/message/push" || total != maxTextRunes*7+10 {
		t.Errorf("%d calls, %d characters", len(calls), total)
	}
}

func TestGroupsAndMentions(t *testing.T) {
	r := newRig(t, nil)
	r.api.profile["/v2/bot/group/G1/member/U1"] = "Ann"
	r.api.profile["/v2/bot/room/R1/member/U2"] = "Bob"

	// chatter in a group is delivered, but not addressed
	r.send(textEvent(groupSrc, "lunch at noon?", nil))
	in := r.next()
	if in.Addressed || !in.IsGroup || in.ChatID != "G1" || in.UserName != "Ann" || in.Text != "lunch at noon?" {
		t.Errorf("%+v", in)
	}
	// a mention of the bot: addressed, and the mention is cut out. Indexes count UTF-16 units,
	// so the emoji before it (two units) and Thai text must not shift the cut.
	text := "😀 @Janny สรุปให้หน่อย @Bob"
	r.send(textEvent(groupSrc, text, map[string]any{"mention": map[string]any{"mentionees": []any{
		map[string]any{"index": 3, "length": 6, "type": "user", "userId": "Ubot", "isSelf": true},
		map[string]any{"index": 24, "length": 4, "type": "user", "userId": "U9", "isSelf": false},
	}}}))
	in = r.next()
	if !in.Addressed || in.Text != "😀  สรุปให้หน่อย @Bob" {
		t.Errorf("addressed=%v text=%q", in.Addressed, in.Text)
	}
	// a mention of someone else only
	r.send(textEvent(groupSrc, "@Bob look", map[string]any{"mention": map[string]any{"mentionees": []any{
		map[string]any{"index": 0, "length": 4, "type": "user", "userId": "U9", "isSelf": false}}}}))
	if in = r.next(); in.Addressed || in.Text != "@Bob look" {
		t.Errorf("%+v", in)
	}
	// commands count as addressed
	r.send(textEvent(groupSrc, "/reset", nil))
	if in = r.next(); !in.Addressed || in.Text != "/reset" {
		t.Errorf("%+v", in)
	}
	// rooms are groups too, with their own member lookup
	r.send(textEvent(map[string]any{"type": "room", "roomId": "R1", "userId": "U2"}, "hi room", nil))
	if in = r.next(); !in.IsGroup || in.ChatID != "R1" || in.UserName != "Bob" {
		t.Errorf("%+v", in)
	}
	// someone who has not agreed to share an identity has no user id and still gets a stable one per group
	r.send(textEvent(map[string]any{"type": "group", "groupId": "G1"}, "who am I", nil))
	if in = r.next(); in.UserID != "unknown:G1" || in.UserName != "user" {
		t.Errorf("%+v", in)
	}
}

func TestNamesAreLookedUpOnceAndFailuresAreHarmless(t *testing.T) {
	r := newRig(t, nil)
	r.api.profile["/v2/bot/profile/U1"] = "Ann"
	r.send(textEvent(userSrc, "one", nil))
	r.next()
	r.api.mu.Lock()
	delete(r.api.profile, "/v2/bot/profile/U1") // the profile call would now fail
	r.api.mu.Unlock()
	r.send(textEvent(userSrc, "two", nil))
	if in := r.next(); in.UserName != "Ann" {
		t.Errorf("the name was not remembered: %q", in.UserName)
	}
	r.advance(2 * time.Hour)
	r.send(textEvent(userSrc, "three", nil))
	if in := r.next(); in.UserName != "user" {
		t.Errorf("a failed lookup must not block the message: %q", in.UserName)
	}
}

func TestImagesFilesAndUnsupportedContent(t *testing.T) {
	r := newRig(t, nil)
	r.api.content["img1"] = []byte("\x89PNG....")
	r.api.content["file1"] = []byte("%PDF-1.4 hi")
	r.api.content["huge"] = make([]byte, 5000)
	ev := func(typ, id string, extra map[string]any) map[string]any {
		e := textEvent(userSrc, "", nil)
		m := map[string]any{"id": id, "type": typ}
		for k, v := range extra {
			m[k] = v
		}
		e["message"] = m
		return e
	}
	r.send(ev("image", "img1", nil), ev("file", "file1", map[string]any{"fileName": "report.pdf", "fileSize": 11}),
		ev("file", "huge", map[string]any{"fileName": "huge.bin", "fileSize": 5000}), ev("video", "v1", nil), ev("sticker", "s1", nil))
	img, file, huge, video, sticker := r.next(), r.next(), r.next(), r.next(), r.next()
	if len(img.Attachments) != 1 || img.Text != "" || !img.Addressed {
		t.Fatalf("%+v", img)
	}
	if d, err := img.Attachments[0].Fetch(context.Background(), 100); err != nil || string(d) != "\x89PNG...." {
		t.Errorf("%q %v", d, err)
	}
	if file.Attachments[0].Name != "report.pdf" || file.Attachments[0].Size != 11 {
		t.Errorf("%+v", file.Attachments)
	}
	if d, err := file.Attachments[0].Fetch(context.Background(), 100); err != nil || string(d) != "%PDF-1.4 hi" {
		t.Errorf("%q %v", d, err)
	}
	if _, err := huge.Attachments[0].Fetch(context.Background(), 100); err != channel.ErrTooLarge {
		t.Errorf("declared size: %v", err)
	}
	if _, err := img.Attachments[0].Fetch(context.Background(), 4); err != channel.ErrTooLarge {
		t.Errorf("a body over the limit: %v", err)
	}
	if !video.HasAttachment || len(video.Attachments) != 0 || !sticker.HasAttachment {
		t.Errorf("%+v %+v", video, sticker)
	}
	gone := r.ch.fetcher("nothing", 0)
	if _, err := gone(context.Background(), 100); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}

func TestOtherEventsAreIgnored(t *testing.T) {
	r := newRig(t, nil)
	r.send(map[string]any{"type": "follow", "webhookEventId": "f1", "replyToken": "x", "source": userSrc},
		map[string]any{"type": "unfollow", "webhookEventId": "f2", "source": userSrc},
		map[string]any{"type": "join", "webhookEventId": "f3", "source": groupSrc},
		map[string]any{"type": "message", "mode": "standby", "webhookEventId": "f4", "source": userSrc, "message": map[string]any{"id": "1", "type": "text", "text": "x"}},
		map[string]any{"type": "message", "webhookEventId": "f5", "source": map[string]any{"type": "unknown"}, "message": map[string]any{"id": "1", "type": "text", "text": "x"}})
	r.none()
}

func TestRedeliveredEventsAreHandledOnce(t *testing.T) {
	r := newRig(t, nil)
	ev := textEvent(userSrc, "once", nil)
	r.send(ev)
	r.next()
	r.send(ev) // LINE delivers again when it believes the first attempt failed
	r.send(ev)
	r.none()
}

func TestEventsOfOneRequestKeepTheirOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	r := newRig(t, func(in channel.Incoming) {
		mu.Lock()
		order = append(order, in.Text)
		mu.Unlock()
	})
	var events []map[string]any
	for i := 0; i < 15; i++ {
		events = append(events, textEvent(userSrc, fmt.Sprintf("m%02d", i), nil))
	}
	r.send(events...)
	for i := 0; i < 15; i++ {
		r.next()
	}
	mu.Lock()
	defer mu.Unlock()
	for i, s := range order {
		if s != fmt.Sprintf("m%02d", i) {
			t.Fatalf("order = %v", order)
		}
	}
}

func TestTheWebhookAnswersBeforeTheMessageIsHandled(t *testing.T) {
	release := make(chan struct{})
	r := newRig(t, func(channel.Incoming) { <-release })
	done := make(chan int, 1)
	go func() { done <- r.send(textEvent(userSrc, "slow work", nil)) }()
	select {
	case code := <-done:
		if code != 200 {
			t.Errorf("code = %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Error("LINE would time out: the webhook waits for the model")
	}
	close(release)
	r.next()
}

func TestTypingOnlyInPrivateChatsAndNotTooOften(t *testing.T) {
	r := newRig(t, nil)
	g := &responder{c: r.ch, chatID: "G1", isGroup: true}
	_ = g.Typing(context.Background())
	p := &responder{c: r.ch, chatID: "U1"}
	for i := 0; i < 5; i++ {
		_ = p.Typing(context.Background())
	}
	calls := r.api.only("/v2/bot/chat/loading/start")
	if len(calls) != 1 || calls[0].Body["chatId"] != "U1" {
		t.Errorf("%+v", calls)
	}
	r.advance(20 * time.Second)
	_ = p.Typing(context.Background())
	if n := len(r.api.only("/v2/bot/chat/loading/start")); n != 2 {
		t.Errorf("%d", n)
	}
}

func TestNotReadyBeforeRun(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", Log: log})
	if _, err := New(Config{ChannelSecret: secret, ChannelToken: token}, srv, log); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body := []byte(`{"events":[]}`)
	req, _ := http.NewRequest("POST", ts.URL+"/webhook/line", bytes.NewReader(body))
	req.Header.Set("X-Line-Signature", sign(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("code = %d: LINE must be told to try again", resp.StatusCode)
	}
	if _, err := New(Config{}, srv, log); err == nil {
		t.Error("a channel without credentials was accepted")
	}
}

func TestUTF16Cut(t *testing.T) {
	for _, c := range []struct {
		text          string
		index, length int
		want          string
	}{
		{"@bot hi", 0, 4, " hi"},
		{"😀@bot hi", 2, 4, "😀 hi"},
		{"ไทย @bot", 4, 4, "ไทย "},
		{"abc", 5, 2, "abc"},
		{"abc", 1, 99, "a"},
		{"abc", -1, 2, "abc"},
		{"abc", 1, 0, "abc"},
	} {
		if got := utf16Cut(c.text, c.index, c.length); got != c.want {
			t.Errorf("utf16Cut(%q, %d, %d) = %q, want %q", c.text, c.index, c.length, got, c.want)
		}
	}
}

func TestNoCommandsDoNotAddressTheBot(t *testing.T) {
	c := &Channel{cfg: Config{NoCommands: true}}
	var ev event
	ev.Type = "message"
	ev.Source.Type, ev.Source.GroupID, ev.Source.UserID = "group", "G1", "U1"
	ev.Message.Type, ev.Message.Text = "text", "/reset"
	in, _, ok := c.convert(ev, time.Now())
	if !ok || in.Addressed || in.Text != "/reset" {
		t.Errorf("ok=%v %+v", ok, in)
	}
}
