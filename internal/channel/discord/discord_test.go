package discord

import (
	"context"
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

	"github.com/coder/websocket"

	"github.com/authapon/jannyq/internal/channel"
)

const token = "SECRET.bot.token"

type gwConn struct {
	conn   *websocket.Conn
	ctx    context.Context
	frames chan map[string]any // frames received from the client
}

func (g *gwConn) send(v any) {
	b, _ := json.Marshal(v)
	_ = g.conn.Write(g.ctx, websocket.MessageText, b)
}

func (g *gwConn) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case f := <-g.frames:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("the client sent nothing in time")
	}
	return nil
}

// nextOp skips heartbeats.
func (g *gwConn) nextOp(t *testing.T, op float64) map[string]any {
	t.Helper()
	for {
		f := g.next(t)
		if f["op"] == op {
			return f
		}
		if f["op"] != float64(1) {
			t.Fatalf("expected op %v, got %v", op, f)
		}
	}
}

type fake struct {
	t   *testing.T
	srv *httptest.Server

	conns chan *gwConn // every gateway connection, as it is accepted

	mu       sync.Mutex
	posts    []map[string]any
	postPath []string
	typing   int
	failNext []int // statuses answered to the next POSTs to /messages
	files    map[string][]byte
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, conns: make(chan *gwConn, 10), files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Query().Get("encoding") == "json":
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		g := &gwConn{conn: c, ctx: ctx, frames: make(chan map[string]any, 100)}
		f.conns <- g
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			g.frames <- m
		}
	case strings.HasPrefix(r.URL.Path, "/cdn/"):
		f.mu.Lock()
		b, ok := f.files[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case strings.HasSuffix(r.URL.Path, "/typing"):
		f.mu.Lock()
		f.typing++
		f.mu.Unlock()
		w.WriteHeader(204)
	case strings.HasSuffix(r.URL.Path, "/messages") && r.Method == "POST":
		if r.Header.Get("Authorization") != "Bot "+token {
			f.t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "DiscordBot (") {
			f.t.Errorf("user agent = %q", r.Header.Get("User-Agent"))
		}
		f.mu.Lock()
		if len(f.failNext) > 0 {
			code := f.failNext[0]
			f.failNext = f.failNext[1:]
			f.mu.Unlock()
			if code == 429 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(429)
				io.WriteString(w, `{"message":"You are being rate limited.","retry_after":0.05,"global":false}`)
				return
			}
			w.WriteHeader(code)
			io.WriteString(w, `{"message":"Missing Permissions","code":50013}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.posts = append(f.posts, body)
		f.postPath = append(f.postPath, r.URL.Path)
		f.mu.Unlock()
		io.WriteString(w, `{"id":"1"}`)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}
}

func (f *fake) channel() *Channel {
	wsURL := "ws" + strings.TrimPrefix(f.srv.URL, "http")
	return New(Config{Token: token, APIBase: f.srv.URL + "/api", GatewayURL: wsURL}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func (f *fake) sentPosts() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.posts...)
}

type runner struct {
	c      *Channel
	cancel context.CancelFunc
	done   chan error
	in     chan channel.Incoming
}

func start(t *testing.T, c *Channel, handle func(channel.Incoming)) *runner {
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{c: c, cancel: cancel, done: make(chan error, 1), in: make(chan channel.Incoming, 50)}
	go func() {
		r.done <- c.Run(ctx, func(ctx context.Context, in channel.Incoming) {
			in.Accepted()
			if handle != nil {
				handle(in)
			}
			r.in <- in
		})
	}()
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

func (r *runner) next(t *testing.T) channel.Incoming {
	t.Helper()
	select {
	case in := <-r.in:
		return in
	case <-time.After(5 * time.Second):
		t.Fatal("no message reached the sink")
	}
	return channel.Incoming{}
}

func (r *runner) none(t *testing.T) {
	t.Helper()
	select {
	case in := <-r.in:
		t.Fatalf("an ignored message reached the sink: %+v", in)
	case <-time.After(200 * time.Millisecond):
	}
}

// handshake plays the gateway's part of connecting: hello, identify, ready.
func handshake(t *testing.T, g *gwConn, interval int, botID string) map[string]any {
	t.Helper()
	g.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": interval}})
	id := g.nextOp(t, 2)
	g.send(map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
		"session_id": "sess-1", "resume_gateway_url": "", "user": map[string]any{"id": botID, "username": "JannyBot", "bot": true},
	}})
	return id
}

func msgEvent(seq int, d map[string]any) map[string]any {
	return map[string]any{"op": 0, "s": seq, "t": "MESSAGE_CREATE", "d": d}
}

func guildMsg(id, text string, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "channel_id": "chan1", "guild_id": "g1", "content": text, "type": 0,
		"timestamp": "2023-11-15T05:13:21.000000+00:00", "author": map[string]any{"id": "u1", "username": "ann", "global_name": "Ann"},
		"member": map[string]any{"nick": ""}, "mentions": []any{}, "attachments": []any{}}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestIdentifyAndAnswerAMention(t *testing.T) {
	f := newFake(t)
	r := start(t, f.channel(), func(in channel.Incoming) {
		_ = in.Responder.Typing(context.Background())
		_ = in.Responder.Send(context.Background(), "hello "+in.UserName)
	})
	g := <-f.conns
	id := handshake(t, g, 60000, "99")
	d := id["d"].(map[string]any)
	if d["token"] != token || d["intents"] != float64(1<<9|1<<12|1<<15) {
		t.Errorf("identify = %v", d)
	}
	g.send(msgEvent(2, guildMsg("m1", "<@99> what is <@5> doing?", map[string]any{
		"mentions": []any{map[string]any{"id": "99", "username": "JannyBot", "bot": true}, map[string]any{"id": "5", "username": "bob", "global_name": "Bob"}},
		"member":   map[string]any{"nick": "Annie"},
	})))
	in := r.next(t)
	if in.Text != "what is @Bob doing?" || !in.IsGroup || !in.Addressed || in.ChatID != "chan1" || in.UserID != "u1" || in.UserName != "Annie" || in.Channel != "discord" {
		t.Errorf("incoming = %+v", in)
	}
	if in.ReceivedAt.Year() != 2023 || in.ReceivedAt.Hour() != 5 {
		t.Errorf("time = %v", in.ReceivedAt)
	}
	posts := f.sentPosts()
	if len(posts) != 1 || posts[0]["content"] != "hello Annie" {
		t.Fatalf("posts = %v", posts)
	}
	ref := posts[0]["message_reference"].(map[string]any)
	am := posts[0]["allowed_mentions"].(map[string]any)
	if ref["message_id"] != "m1" || ref["fail_if_not_exists"] != false || len(am["parse"].([]any)) != 0 {
		t.Errorf("a reply must quote the message and never ping: %v", posts[0])
	}
	f.mu.Lock()
	typing, path := f.typing, f.postPath[0]
	f.mu.Unlock()
	if typing != 1 || path != "/api/channels/chan1/messages" {
		t.Errorf("typing=%d path=%q", typing, path)
	}
}

func TestWhoIsAddressed(t *testing.T) {
	f := newFake(t)
	r := start(t, f.channel(), nil)
	g := <-f.conns
	handshake(t, g, 60000, "99")
	bot := map[string]any{"id": "99", "username": "JannyBot", "bot": true}

	cases := []struct {
		name      string
		m         map[string]any
		want      bool
		ignored   bool
		wantText  string
		wantGroup bool
	}{
		{"chatter", guildMsg("1", "lunch at noon?", nil), false, false, "lunch at noon?", true},
		{"dm", map[string]any{"id": "2", "channel_id": "dm1", "content": "hi", "type": 0, "author": map[string]any{"id": "u1", "username": "ann"}, "mentions": []any{}}, true, false, "hi", false},
		{"nick mention", guildMsg("3", "hey <@!99> help", map[string]any{"mentions": []any{bot}}), true, false, "hey  help", true},
		{"reply to the bot", guildMsg("4", "and then?", map[string]any{"type": 19, "referenced_message": map[string]any{"id": "x", "author": bot}}), true, false, "and then?", true},
		{"reply to a person", guildMsg("5", "agree", map[string]any{"type": 19, "referenced_message": map[string]any{"id": "x", "author": map[string]any{"id": "u2", "username": "bob"}}}), false, false, "agree", true},
		{"bang command", guildMsg("6", "!reset", nil), true, false, "/reset", true},
		{"bang command with arguments", guildMsg("7", "!Compact now", nil), true, false, "/compact now", true},
		{"unknown bang", guildMsg("8", "!important meeting", nil), false, false, "!important meeting", true},
		{"slash text in a server", guildMsg("9", "/help", nil), true, false, "/help", true},
		{"from a bot", guildMsg("10", "beep", map[string]any{"author": map[string]any{"id": "u9", "username": "other", "bot": true}}), false, true, "", true},
		{"from a webhook", guildMsg("11", "hook", map[string]any{"webhook_id": "w1"}), false, true, "", true},
		{"from itself", guildMsg("12", "echo", map[string]any{"author": bot}), false, true, "", true},
		{"a join notice", guildMsg("13", "", map[string]any{"type": 7}), false, true, "", true},
		{"empty (embed only)", guildMsg("14", "", nil), false, true, "", true},
	}
	for i, c := range cases {
		g.send(msgEvent(10+i, c.m))
		if c.ignored {
			r.none(t)
			continue
		}
		in := r.next(t)
		if in.Addressed != c.want || in.Text != c.wantText || in.IsGroup != c.wantGroup {
			t.Errorf("%s: addressed=%v text=%q group=%v", c.name, in.Addressed, in.Text, in.IsGroup)
		}
	}
}

func TestHeartbeatsCarryTheSequenceAndADeadConnectionIsReplaced(t *testing.T) {
	f := newFake(t)
	start(t, f.channel(), nil)
	g := <-f.conns
	g.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 80}})
	g.nextOp(t, 2)
	g.send(map[string]any{"op": 0, "s": 7, "t": "READY", "d": map[string]any{"session_id": "S", "user": map[string]any{"id": "99", "username": "b"}}})
	hb := g.next(t)
	if hb["op"] != float64(1) {
		t.Fatalf("expected a heartbeat, got %v", hb)
	}
	// acknowledge the first one, then stay silent: the second beat finds no acknowledgement
	g.send(map[string]any{"op": 11})
	var second *gwConn
	select {
	case second = <-f.conns:
	case <-time.After(5 * time.Second):
		t.Fatal("a connection without heartbeat acknowledgements was not replaced")
	}
	// the new connection resumes the old session instead of starting over
	second.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
	res := second.nextOp(t, 6)["d"].(map[string]any)
	if res["session_id"] != "S" || res["seq"] != float64(7) || res["token"] != token {
		t.Errorf("resume = %v", res)
	}
}

func TestReconnectRequestResumesAndInvalidSessionStartsOver(t *testing.T) {
	f := newFake(t)
	r := start(t, f.channel(), nil)
	g := <-f.conns
	handshake(t, g, 60000, "99")
	g.send(msgEvent(2, guildMsg("1", "one", nil)))
	r.next(t)
	g.send(map[string]any{"op": 7})
	g2 := <-f.conns
	g2.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
	res := g2.nextOp(t, 6)["d"].(map[string]any)
	if res["session_id"] != "sess-1" || res["seq"] != float64(2) {
		t.Fatalf("resume = %v", res)
	}
	g2.send(map[string]any{"op": 0, "s": 3, "t": "RESUMED", "d": map[string]any{}})
	g2.send(msgEvent(4, guildMsg("2", "two", nil)))
	if in := r.next(t); in.Text != "two" {
		t.Errorf("after resuming: %+v", in)
	}
	// the session cannot be resumed: identify again, from scratch
	g2.send(map[string]any{"op": 9, "d": false})
	g3 := <-f.conns
	g3.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
	g3.nextOp(t, 2)
}

func TestFatalCloseCodesStopTheChannelWithAHelpfulError(t *testing.T) {
	for code, want := range map[websocket.StatusCode]string{4014: "MESSAGE CONTENT", 4004: "token"} {
		f := newFake(t)
		r := start(t, f.channel(), nil)
		g := <-f.conns
		g.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
		g.nextOp(t, 2)
		_ = g.conn.Close(code, "x")
		select {
		case err := <-r.done:
			if err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "SECRET") {
				t.Errorf("code %d: err = %v", code, err)
			}
			r.done <- err // for the cleanup
		case <-time.After(5 * time.Second):
			t.Fatalf("code %d: the channel kept retrying", code)
		}
	}
}

func TestTransientDisconnectsAreRetried(t *testing.T) {
	f := newFake(t)
	start(t, f.channel(), nil)
	g := <-f.conns
	g.send(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}})
	g.nextOp(t, 2)
	_ = g.conn.Close(websocket.StatusGoingAway, "restart")
	select {
	case <-f.conns:
	case <-time.After(10 * time.Second):
		t.Fatal("no reconnection")
	}
}

func TestReplyLimitsAndErrors(t *testing.T) {
	f := newFake(t)
	c := f.channel()
	r := &responder{c: c, channelID: "chan1", replyTo: "m1", isGroup: true}
	long := strings.Repeat("ก", maxMessageRunes) + strings.Repeat("x", 500)
	if err := r.Send(context.Background(), long); err != nil {
		t.Fatal(err)
	}
	posts := f.sentPosts()
	if len(posts) != 2 {
		t.Fatalf("%d posts", len(posts))
	}
	for _, p := range posts {
		if n := len([]rune(p["content"].(string))); n > 2000 {
			t.Errorf("a message of %d characters", n)
		}
	}
	if posts[0]["message_reference"] == nil || posts[1]["message_reference"] != nil {
		t.Error("only the first part quotes the message")
	}
	// private chats do not quote
	f.mu.Lock()
	f.posts = nil
	f.mu.Unlock()
	_ = (&responder{c: c, channelID: "dm", replyTo: "m", isGroup: false}).Send(context.Background(), "hi")
	if p := f.sentPosts(); len(p) != 1 || p[0]["message_reference"] != nil {
		t.Errorf("%v", p)
	}
	// rate limits are waited out
	f.mu.Lock()
	f.failNext = []int{429, 429}
	f.mu.Unlock()
	start := time.Now()
	if err := r.Send(context.Background(), "after the limit"); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 80*time.Millisecond {
		t.Error("the retry_after was not honoured")
	}
	// a refusal is an error that does not leak the token
	f.mu.Lock()
	f.failNext = []int{403}
	f.mu.Unlock()
	err := r.Send(context.Background(), "nope")
	if err == nil || !strings.Contains(err.Error(), "403") || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}

func TestAttachments(t *testing.T) {
	f := newFake(t)
	f.files["/cdn/report.pdf"] = []byte("%PDF-1.4 hello")
	f.files["/cdn/big.bin"] = make([]byte, 5000)
	r := start(t, f.channel(), nil)
	g := <-f.conns
	handshake(t, g, 60000, "99")
	g.send(msgEvent(2, guildMsg("m1", "<@99> read this", map[string]any{
		"mentions": []any{map[string]any{"id": "99", "username": "JannyBot", "bot": true}},
		"attachments": []any{
			map[string]any{"id": "a1", "filename": "report.pdf", "content_type": "application/pdf", "size": 14, "url": f.srv.URL + "/cdn/report.pdf"},
			map[string]any{"id": "a2", "filename": "big.bin", "size": 5000, "url": f.srv.URL + "/cdn/big.bin"},
			map[string]any{"id": "a3", "filename": "gone.txt", "size": 3, "url": f.srv.URL + "/cdn/gone.txt"},
		},
	})))
	in := r.next(t)
	if len(in.Attachments) != 3 || in.Attachments[0].Name != "report.pdf" || in.Attachments[0].MIME != "application/pdf" {
		t.Fatalf("%+v", in.Attachments)
	}
	data, err := in.Attachments[0].Fetch(context.Background(), 1000)
	if err != nil || string(data) != "%PDF-1.4 hello" {
		t.Errorf("%q %v", data, err)
	}
	if _, err := in.Attachments[1].Fetch(context.Background(), 1000); err != channel.ErrTooLarge {
		t.Errorf("declared size over the limit: %v", err)
	}
	if _, err := in.Attachments[2].Fetch(context.Background(), 1000); err == nil {
		t.Error("a missing file was not an error")
	}
	// a picture without text still reaches the bot in a direct chat
	g.send(msgEvent(3, map[string]any{"id": "m2", "channel_id": "dm1", "content": "", "type": 0, "author": map[string]any{"id": "u1", "username": "ann"},
		"attachments": []any{map[string]any{"id": "a4", "filename": "pic.png", "size": 3, "url": f.srv.URL + "/cdn/report.pdf"}}}))
	if in = r.next(t); in.Text != "" || len(in.Attachments) != 1 || !in.Addressed {
		t.Errorf("%+v", in)
	}
	// a lone body larger than the declared size is cut off
	f.mu.Lock()
	f.files["/cdn/liar.bin"] = make([]byte, 5000)
	f.mu.Unlock()
	if _, err := f.channel().fetcher(f.srv.URL+"/cdn/liar.bin", 10)(context.Background(), 1000); err != channel.ErrTooLarge {
		t.Errorf("understated size: %v", err)
	}
}

func TestMessagesOfAChatKeepTheirOrder(t *testing.T) {
	f := newFake(t)
	var mu sync.Mutex
	var order []string
	r := start(t, f.channel(), func(in channel.Incoming) {
		mu.Lock()
		order = append(order, in.Text)
		mu.Unlock()
	})
	g := <-f.conns
	handshake(t, g, 60000, "99")
	for i := 0; i < 20; i++ {
		g.send(msgEvent(2+i, guildMsg(fmt.Sprint(i), fmt.Sprintf("m%02d", i), nil)))
	}
	for i := 0; i < 20; i++ {
		r.next(t)
	}
	mu.Lock()
	defer mu.Unlock()
	for i, s := range order {
		if s != fmt.Sprintf("m%02d", i) {
			t.Fatalf("order = %v", order)
		}
	}
}

func TestNoCommandsLeavesBangAndSlashTextAlone(t *testing.T) {
	c := New(Config{Token: "t", NoCommands: true}, nil)
	for _, text := range []string{"!reset", "!help", "/compact"} {
		in, ok := c.convert(&message{
			ID: "1", ChannelID: "c1", GuildID: "g1", Content: text,
			Author: user{ID: "u1", Username: "ann"},
		})
		if !ok || in.Text != text || in.Addressed {
			t.Errorf("%q: ok=%v text=%q addressed=%v", text, ok, in.Text, in.Addressed)
		}
	}
}

func TestNotifierSendsToTheChannel(t *testing.T) {
	f := newFake(t)
	var n channel.Notifier = f.channel()
	r, err := n.ResponderFor("chan9", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Send(context.Background(), "scheduled"); err != nil {
		t.Fatal(err)
	}
	p := f.sentPosts()
	if len(p) != 1 || p[0]["content"] != "scheduled" || p[0]["message_reference"] != nil {
		t.Errorf("%v", p)
	}
	if _, err := n.ResponderFor("", false); err == nil {
		t.Error("an empty channel id")
	}
}
