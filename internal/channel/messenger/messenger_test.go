package messenger

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
	appSecret = "app-secret-123"
	pageToken = "PAGE.ACCESS.TOKEN"
)

type fakeGraph struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	posts    []map[string]any
	profiles map[string]string // psid -> name
	files    map[string][]byte
	cdnAuth  []string // Authorization headers seen on CDN downloads
	failSend bool
}

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{t: t, profiles: map[string]string{}, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, "/cdn/"):
		f.cdnAuth = append(f.cdnAuth, r.Header.Get("Authorization"))
		b, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	case r.Method == "POST" && r.URL.Path == "/v1/me/messages":
		if r.Header.Get("Authorization") != "Bearer "+pageToken || r.URL.RawQuery != "" {
			f.t.Errorf("auth %q query %q", r.Header.Get("Authorization"), r.URL.RawQuery)
		}
		if f.failSend {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"(#10) This message is sent outside of allowed window.","code":10,"error_subcode":2018278}}`)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.posts = append(f.posts, b)
		io.WriteString(w, `{"recipient_id":"x","message_id":"m"}`)
	case r.Method == "GET":
		psid := strings.TrimPrefix(r.URL.Path, "/v1/")
		name, ok := f.profiles[psid]
		if !ok {
			w.WriteHeader(403)
			io.WriteString(w, `{"error":{"message":"(#230) Requires pages_messaging permission","code":230}}`)
			return
		}
		fmt.Fprintf(w, `{"name":%q}`, name)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}
}

func (f *fakeGraph) sent() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.posts...)
}

type rig struct {
	t   *testing.T
	g   *fakeGraph
	web *httptest.Server
	ch  *Channel
	in  chan channel.Incoming
}

func newRig(t *testing.T, handle func(channel.Incoming)) *rig {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	g := newFakeGraph(t)
	srv := server.New(server.Options{Addr: ":0", Log: log})
	ch, err := New(Config{PageToken: pageToken, AppSecret: appSecret, VerifyToken: "vt", GraphAPI: g.srv.URL + "/v1"}, srv, log)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, g: g, ch: ch, web: httptest.NewServer(srv.Handler()), in: make(chan channel.Incoming, 50)}
	t.Cleanup(r.web.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = ch.Run(ctx, func(_ context.Context, in channel.Incoming) {
			in.Accepted()
			if handle != nil {
				handle(in)
			}
			r.in <- in
		})
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func (r *rig) post(body any) int {
	b, _ := json.Marshal(body)
	m := hmac.New(sha256.New, []byte(appSecret))
	m.Write(b)
	req, _ := http.NewRequest("POST", r.web.URL+"/webhook/messenger", strings.NewReader(string(b)))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(m.Sum(nil)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (r *rig) next() channel.Incoming {
	select {
	case in := <-r.in:
		return in
	case <-time.After(5 * time.Second):
		r.t.Fatal("nothing arrived")
	}
	return channel.Incoming{}
}

func (r *rig) none() {
	select {
	case in := <-r.in:
		r.t.Fatalf("unexpected: %+v", in)
	case <-time.After(150 * time.Millisecond):
	}
}

var seq int

func msg(sender string, m map[string]any) map[string]any {
	seq++
	if m["mid"] == nil {
		m["mid"] = fmt.Sprintf("mid.%d", seq)
	}
	return map[string]any{"sender": map[string]any{"id": sender}, "recipient": map[string]any{"id": "PAGE"}, "timestamp": 1700000000123, "message": m}
}

func hook(events ...map[string]any) map[string]any {
	return map[string]any{"object": "page", "entry": []any{map[string]any{"id": "PAGE", "time": 1, "messaging": events}}}
}

func TestMessageAndReply(t *testing.T) {
	r := newRig(t, func(in channel.Incoming) {
		_ = in.Responder.Typing(context.Background())
		_ = in.Responder.Send(context.Background(), "hello "+in.UserName)
	})
	r.g.profiles["PSID1"] = "Ann Lee"
	if code := r.post(hook(msg("PSID1", map[string]any{"text": " สวัสดี "}))); code != 200 {
		t.Fatal(code)
	}
	in := r.next()
	if in.Text != "สวัสดี" || in.Channel != "messenger" || in.ChatID != "PSID1" || in.UserID != "PSID1" || in.UserName != "Ann Lee" || !in.Addressed || in.IsGroup {
		t.Errorf("%+v", in)
	}
	if !in.ReceivedAt.Equal(time.UnixMilli(1700000000123)) {
		t.Errorf("time = %v", in.ReceivedAt)
	}
	posts := r.g.sent()
	if len(posts) != 2 || posts[0]["sender_action"] != "typing_on" {
		t.Fatalf("posts = %v", posts)
	}
	m := posts[1]["message"].(map[string]any)
	if posts[1]["messaging_type"] != "RESPONSE" || m["text"] != "hello Ann Lee" || posts[1]["recipient"].(map[string]any)["id"] != "PSID1" {
		t.Errorf("reply = %v", posts[1])
	}
}

func TestNamesAreOptional(t *testing.T) {
	r := newRig(t, nil)
	r.post(hook(msg("PSID9", map[string]any{"text": "no permission to read my name"})))
	if in := r.next(); in.UserName != "user" {
		t.Errorf("name = %q", in.UserName)
	}
	r.g.mu.Lock()
	r.g.profiles["PSID2"] = "Bo"
	r.g.mu.Unlock()
	r.post(hook(msg("PSID2", map[string]any{"text": "one"})))
	r.next()
	r.g.mu.Lock()
	delete(r.g.profiles, "PSID2")
	r.g.mu.Unlock()
	r.post(hook(msg("PSID2", map[string]any{"text": "two"})))
	if in := r.next(); in.UserName != "Bo" {
		t.Errorf("the name was not remembered: %q", in.UserName)
	}
}

func TestOnlyRealMessagesFromPeopleAreHandled(t *testing.T) {
	r := newRig(t, nil)
	echo := msg("PAGE", map[string]any{"text": "my own reply", "is_echo": true})
	fromPage := msg("PAGE", map[string]any{"text": "from the page id"})
	receipt := map[string]any{"sender": map[string]any{"id": "PSID1"}, "recipient": map[string]any{"id": "PAGE"}, "delivery": map[string]any{"mids": []string{"m"}}}
	postback := map[string]any{"sender": map[string]any{"id": "PSID1"}, "recipient": map[string]any{"id": "PAGE"}, "postback": map[string]any{"payload": "x"}}
	noSender := map[string]any{"recipient": map[string]any{"id": "PAGE"}, "message": map[string]any{"mid": "z", "text": "?"}}
	r.post(hook(echo, fromPage, receipt, postback, noSender))
	r.none()
	// other kinds of subscription are acknowledged and ignored
	if code := r.post(map[string]any{"object": "instagram", "entry": []any{map[string]any{"id": "1", "messaging": []any{msg("PSID1", map[string]any{"text": "x"})}}}}); code != 200 {
		t.Errorf("code = %d", code)
	}
	r.none()
}

func TestAttachments(t *testing.T) {
	r := newRig(t, nil)
	r.g.files["/cdn/photo.jpg"] = []byte("JPEGDATA")
	r.g.files["/cdn/files/Quarterly%20Report.pdf"] = []byte("%PDF-1.4 hi")
	r.g.files["/cdn/files/Quarterly Report.pdf"] = []byte("%PDF-1.4 hi")
	cdn := r.g.srv.URL + "/cdn"
	att := func(typ, u string) map[string]any {
		return map[string]any{"type": typ, "payload": map[string]any{"url": u}}
	}
	r.post(hook(
		msg("PSID1", map[string]any{"mid": "mid.AbC123xyz", "attachments": []any{att("image", cdn+"/photo.jpg?sig=1")}}),
		msg("PSID1", map[string]any{"attachments": []any{att("file", cdn+"/files/Quarterly%20Report.pdf?sig=2")}, "text": "see attached"}),
		msg("PSID1", map[string]any{"attachments": []any{att("audio", cdn+"/a.mp4"), att("fallback", "https://example.com")}}),
		msg("PSID1", map[string]any{"attachments": []any{att("fallback", "https://example.com/article")}, "text": "look at this link"}),
	))
	img, file, audio, link := r.next(), r.next(), r.next(), r.next()
	if len(img.Attachments) != 1 || img.Text != "" || !strings.HasPrefix(img.Attachments[0].Name, "image-") || !strings.HasSuffix(img.Attachments[0].Name, ".jpg") {
		t.Fatalf("%+v", img)
	}
	if d, err := img.Attachments[0].Fetch(context.Background(), 100); err != nil || string(d) != "JPEGDATA" {
		t.Errorf("%q %v", d, err)
	}
	if file.Text != "see attached" || len(file.Attachments) != 1 || file.Attachments[0].Name != "Quarterly Report.pdf" {
		t.Fatalf("%+v", file)
	}
	if d, err := file.Attachments[0].Fetch(context.Background(), 100); err != nil || string(d) != "%PDF-1.4 hi" {
		t.Errorf("%q %v", d, err)
	}
	if _, err := img.Attachments[0].Fetch(context.Background(), 3); err != channel.ErrTooLarge {
		t.Errorf("limit: %v", err)
	}
	if !audio.HasAttachment || len(audio.Attachments) != 0 {
		t.Errorf("%+v", audio)
	}
	if link.HasAttachment || link.Text != "look at this link" {
		t.Errorf("a shared link is not a file: %+v", link)
	}
	// the page token must never be sent to the CDN
	r.g.mu.Lock()
	defer r.g.mu.Unlock()
	for _, a := range r.g.cdnAuth {
		if a != "" {
			t.Errorf("an Authorization header reached the CDN: %q", a)
		}
	}
	if len(r.g.cdnAuth) == 0 {
		t.Error("nothing was downloaded")
	}
}

func TestLongRepliesAreSplit(t *testing.T) {
	r := newRig(t, nil)
	rs := &responder{c: r.ch, psid: "P"}
	if err := rs.Send(context.Background(), strings.Repeat("ก", maxTextRunes*2+7)); err != nil {
		t.Fatal(err)
	}
	posts := r.g.sent()
	if len(posts) != 3 {
		t.Fatalf("%d posts", len(posts))
	}
	for _, p := range posts {
		if n := len([]rune(p["message"].(map[string]any)["text"].(string))); n > 2000 {
			t.Errorf("a message of %d characters", n)
		}
	}
}

func TestSendErrorsAreReportedWithoutTheToken(t *testing.T) {
	r := newRig(t, nil)
	r.g.failSend = true
	err := (&responder{c: r.ch, psid: "P"}).Send(context.Background(), "late")
	if err == nil || !strings.Contains(err.Error(), "outside of allowed window") || strings.Contains(err.Error(), "ACCESS") {
		t.Errorf("err = %v", err)
	}
}

func TestSignatureRequired(t *testing.T) {
	r := newRig(t, nil)
	b, _ := json.Marshal(hook(msg("PSID1", map[string]any{"text": "forged"})))
	req, _ := http.NewRequest("POST", r.web.URL+"/webhook/messenger", strings.NewReader(string(b)))
	req.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("code = %d", resp.StatusCode)
	}
	r.none()
	resp, err = http.Get(r.web.URL + "/webhook/messenger?hub.mode=subscribe&hub.verify_token=vt&hub.challenge=777")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "777" {
		t.Errorf("handshake: %q", body)
	}
}

func TestNewRequiresSecrets(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := New(Config{PageToken: pageToken}, server.New(server.Options{Addr: ":0", Log: log}), log); err == nil {
		t.Error("no app secret and verify token was accepted")
	}
}

func TestAttachmentNames(t *testing.T) {
	for _, c := range []struct{ url, kind, want string }{
		{"https://cdn.example/files/report.pdf?sig=1", "file", "report.pdf"},
		{"https://cdn.example/files/My%20Notes.txt", "file", "My Notes.txt"},
		{"https://cdn.example/blob", "file", "file-"},
		{"https://cdn.example/a/b.png?x=1", "image", "image-"},
	} {
		got, _ := attachmentName(c.url, c.kind, "mid.AbCdEfGh12345", 0)
		if !strings.HasPrefix(got, c.want) {
			t.Errorf("%s: %q, want prefix %q", c.url, got, c.want)
		}
	}
}
