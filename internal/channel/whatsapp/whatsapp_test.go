package whatsapp

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
	token     = "WA.ACCESS.TOKEN"
	phoneID   = "1055555"
)

type fakeGraph struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	posts    []map[string]any
	media    map[string][]byte
	sizes    map[string]int64
	cdnAuth  []string
	failSend bool
}

func newFakeGraph(t *testing.T) *fakeGraph {
	f := &fakeGraph{t: t, media: map[string][]byte{}, sizes: map[string]int64{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGraph) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+token {
		f.t.Errorf("authorization %q on %s", r.Header.Get("Authorization"), r.URL.Path)
	}
	switch {
	case r.URL.Path == "/v1/"+phoneID+"/messages":
		if f.failSend {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"Re-engagement message","code":131047}}`)
			return
		}
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.posts = append(f.posts, b)
		io.WriteString(w, `{"messages":[{"id":"wamid.out"}]}`)
	case strings.HasPrefix(r.URL.Path, "/dl/"):
		f.cdnAuth = append(f.cdnAuth, r.Header.Get("Authorization"))
		w.Write(f.media[strings.TrimPrefix(r.URL.Path, "/dl/")])
	case strings.HasPrefix(r.URL.Path, "/v1/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/")
		b, ok := f.media[id]
		if !ok {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"message":"Unsupported get request","code":100}}`)
			return
		}
		size := int64(len(b))
		if s, ok := f.sizes[id]; ok {
			size = s
		}
		fmt.Fprintf(w, `{"url":%q,"file_size":%d,"mime_type":"x"}`, f.srv.URL+"/dl/"+id, size)
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
	ch, err := New(Config{AccessToken: token, PhoneNumberID: phoneID, AppSecret: appSecret, VerifyToken: "vt", GraphAPI: g.srv.URL + "/v1"}, srv, log)
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
	req, _ := http.NewRequest("POST", r.web.URL+"/webhook/whatsapp", strings.NewReader(string(b)))
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

func message(from, typ string, extra map[string]any) map[string]any {
	seq++
	m := map[string]any{"from": from, "id": fmt.Sprintf("wamid.%d", seq), "timestamp": "1700000000", "type": typ}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func hook(phone string, contacts []any, messages ...map[string]any) map[string]any {
	return map[string]any{"object": "whatsapp_business_account", "entry": []any{map[string]any{"id": "WABA", "changes": []any{map[string]any{
		"field": "messages", "value": map[string]any{"messaging_product": "whatsapp", "metadata": map[string]any{"phone_number_id": phone, "display_phone_number": "66 1"},
			"contacts": contacts, "messages": messages}}}}}}
}

func contact(wa, name string) any {
	return map[string]any{"wa_id": wa, "profile": map[string]any{"name": name}}
}

func TestTextAndReply(t *testing.T) {
	r := newRig(t, func(in channel.Incoming) {
		_ = in.Responder.Typing(context.Background())
		_ = in.Responder.Send(context.Background(), "hello "+in.UserName)
	})
	m := message("66812345678", "text", map[string]any{"text": map[string]any{"body": "  สวัสดีครับ "}})
	if code := r.post(hook(phoneID, []any{contact("66812345678", "Somchai")}, m)); code != 200 {
		t.Fatal(code)
	}
	in := r.next()
	if in.Text != "สวัสดีครับ" || in.Channel != "whatsapp" || in.ChatID != "66812345678" || in.UserID != "66812345678" || in.UserName != "Somchai" || !in.Addressed || in.IsGroup {
		t.Errorf("%+v", in)
	}
	if !in.ReceivedAt.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("time = %v", in.ReceivedAt)
	}
	posts := r.g.sent()
	if len(posts) != 2 {
		t.Fatalf("posts = %v", posts)
	}
	if posts[0]["status"] != "read" || posts[0]["message_id"] != m["id"] || posts[0]["typing_indicator"].(map[string]any)["type"] != "text" {
		t.Errorf("typing = %v", posts[0])
	}
	if posts[1]["to"] != "66812345678" || posts[1]["type"] != "text" || posts[1]["messaging_product"] != "whatsapp" ||
		posts[1]["text"].(map[string]any)["body"] != "hello Somchai" || posts[1]["text"].(map[string]any)["preview_url"] != false {
		t.Errorf("reply = %v", posts[1])
	}
}

func TestOnlyMessagesToThisNumberAreHandled(t *testing.T) {
	r := newRig(t, nil)
	text := func() map[string]any {
		return message("1", "text", map[string]any{"text": map[string]any{"body": "hi"}})
	}
	// a message for another number of the same account
	r.post(hook("999", nil, text()))
	r.none()
	// status updates only
	r.post(map[string]any{"object": "whatsapp_business_account", "entry": []any{map[string]any{"changes": []any{map[string]any{"field": "messages",
		"value": map[string]any{"metadata": map[string]any{"phone_number_id": phoneID}, "statuses": []any{map[string]any{"id": "x", "status": "delivered"}}}}}}}})
	r.none()
	// other kinds of change, objects, reactions
	r.post(map[string]any{"object": "page", "entry": []any{}})
	r.post(map[string]any{"object": "whatsapp_business_account", "entry": []any{map[string]any{"changes": []any{map[string]any{"field": "account_update", "value": map[string]any{}}}}}})
	r.post(hook(phoneID, nil, message("1", "reaction", map[string]any{"reaction": map[string]any{"emoji": "👍"}})))
	r.none()
	// and the right one still works
	r.post(hook(phoneID, nil, text()))
	if in := r.next(); in.UserName != "user" || in.Text != "hi" {
		t.Errorf("%+v", in)
	}
}

func TestMediaAndTheTokenGoesToMetaOnly(t *testing.T) {
	r := newRig(t, nil)
	r.g.media["img1"] = []byte("JPEGDATA")
	r.g.media["doc1"] = []byte("%PDF-1.4 hello")
	r.g.media["big"] = make([]byte, 5000)
	r.g.sizes["big"] = 5000
	r.post(hook(phoneID, []any{contact("1", "A")},
		message("1", "image", map[string]any{"image": map[string]any{"id": "img1", "mime_type": "image/jpeg", "caption": "what is this?"}}),
		message("1", "document", map[string]any{"document": map[string]any{"id": "doc1", "filename": "report.pdf", "mime_type": "application/pdf"}}),
		message("1", "document", map[string]any{"document": map[string]any{"id": "big", "filename": "big.bin"}}),
		message("1", "audio", map[string]any{"audio": map[string]any{"id": "a1"}}),
		message("1", "sticker", map[string]any{"sticker": map[string]any{"id": "s1"}}),
		message("1", "location", map[string]any{"location": map[string]any{"latitude": 1}}),
		message("1", "image", nil), // an image message without the image
	))
	img, doc, big, audio, sticker, loc, broken := r.next(), r.next(), r.next(), r.next(), r.next(), r.next(), r.next()
	if img.Text != "what is this?" || len(img.Attachments) != 1 || !strings.HasPrefix(img.Attachments[0].Name, "image-") || img.Attachments[0].MIME != "image/jpeg" {
		t.Fatalf("%+v", img)
	}
	if d, err := img.Attachments[0].Fetch(context.Background(), 100); err != nil || string(d) != "JPEGDATA" {
		t.Errorf("%q %v", d, err)
	}
	if doc.Attachments[0].Name != "report.pdf" || doc.Attachments[0].MIME != "application/pdf" {
		t.Errorf("%+v", doc.Attachments)
	}
	if d, err := doc.Attachments[0].Fetch(context.Background(), 100); err != nil || string(d) != "%PDF-1.4 hello" {
		t.Errorf("%q %v", d, err)
	}
	if _, err := big.Attachments[0].Fetch(context.Background(), 1000); err != channel.ErrTooLarge {
		t.Errorf("declared size: %v", err)
	}
	if _, err := img.Attachments[0].Fetch(context.Background(), 4); err != channel.ErrTooLarge {
		t.Errorf("body over the limit: %v", err)
	}
	for name, in := range map[string]channel.Incoming{"audio": audio, "sticker": sticker, "location": loc, "image without id": broken} {
		if !in.HasAttachment || len(in.Attachments) != 0 {
			t.Errorf("%s: %+v", name, in)
		}
	}
	// the address of a media file comes from Meta, and needs the token
	r.g.mu.Lock()
	auths := append([]string(nil), r.g.cdnAuth...)
	r.g.mu.Unlock()
	if len(auths) == 0 {
		t.Fatal("nothing downloaded")
	}
	for _, a := range auths {
		if a != "Bearer "+token {
			t.Errorf("download without the token: %q", a)
		}
	}
	// an unknown media id is an error, and not one that carries the token
	if _, err := r.ch.fetcher("nope")(context.Background(), 100); err == nil || strings.Contains(err.Error(), "ACCESS") {
		t.Errorf("err = %v", err)
	}
}

func TestLongRepliesAndErrors(t *testing.T) {
	r := newRig(t, nil)
	rs := &responder{c: r.ch, to: "1", messageID: "w"}
	if err := rs.Send(context.Background(), strings.Repeat("ก", maxTextRunes+10)); err != nil {
		t.Fatal(err)
	}
	posts := r.g.sent()
	if len(posts) != 2 {
		t.Fatalf("%d posts", len(posts))
	}
	for _, p := range posts {
		if n := len([]rune(p["text"].(map[string]any)["body"].(string))); n > 4096 {
			t.Errorf("a message of %d characters", n)
		}
	}
	r.g.failSend = true
	err := rs.Send(context.Background(), "after the 24 hour window")
	if err == nil || !strings.Contains(err.Error(), "131047") || strings.Contains(err.Error(), "ACCESS") {
		t.Errorf("err = %v", err)
	}
}

func TestSignatureAndHandshake(t *testing.T) {
	r := newRig(t, nil)
	b, _ := json.Marshal(hook(phoneID, nil, message("1", "text", map[string]any{"text": map[string]any{"body": "forged"}})))
	req, _ := http.NewRequest("POST", r.web.URL+"/webhook/whatsapp", strings.NewReader(string(b)))
	req.Header.Set("X-Hub-Signature-256", "sha256=00")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("code = %d", resp.StatusCode)
	}
	r.none()
	resp, _ = http.Get(r.web.URL + "/webhook/whatsapp?hub.mode=subscribe&hub.verify_token=vt&hub.challenge=4242")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "4242" {
		t.Errorf("handshake %q", body)
	}
}

func TestRedeliveryIsIgnored(t *testing.T) {
	r := newRig(t, nil)
	h := hook(phoneID, nil, message("1", "text", map[string]any{"text": map[string]any{"body": "once"}}))
	r.post(h)
	r.next()
	r.post(h)
	r.none()
}
