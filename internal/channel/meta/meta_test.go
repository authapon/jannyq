package meta

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	appSecret   = "app-secret-123"
	verifyToken = "verify-me"
)

func sign(body string) string {
	m := hmac.New(sha256.New, []byte(appSecret))
	m.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

type rig struct {
	t   *testing.T
	web *httptest.Server
	rc  *Receiver
	in  chan channel.Incoming
}

// parse reads {"items":[{"id":"..","chat":"..","text":".."}]}.
func parse(body []byte) ([]Item, error) {
	var p struct {
		Items []struct{ ID, Chat, Text string } `json:"items"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	var out []Item
	for _, i := range p.Items {
		out = append(out, Item{Key: i.ID, Chat: i.Chat, Build: func(context.Context) channel.Incoming {
			return channel.Incoming{Channel: "t", ChatID: i.Chat, Text: i.Text}
		}})
	}
	return out, nil
}

func newRig(t *testing.T, handle func(channel.Incoming)) *rig { return newRigLimit(t, handle, 100) }

func newRigLimit(t *testing.T, handle func(channel.Incoming), bad int) *rig {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", RatePerMinute: 2, Log: log})
	rc, err := NewReceiver(ReceiverConfig{Name: "t", Path: "/hook", AppSecret: appSecret, VerifyToken: verifyToken, Parse: parse, BadPerMinute: bad}, srv, log)
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, rc: rc, web: httptest.NewServer(srv.Handler()), in: make(chan channel.Incoming, 100)}
	t.Cleanup(r.web.Close)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = rc.Run(ctx, func(_ context.Context, in channel.Incoming) {
			if handle != nil {
				handle(in)
			}
			in.Accepted()
			r.in <- in
		})
		close(done)
	}()
	for i := 0; i < 200; i++ {
		rc.mu.Lock()
		ok := rc.sink != nil
		rc.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Cleanup(func() { cancel(); <-done })
	return r
}

func (r *rig) post(body string, hdr map[string]string) int {
	req, _ := http.NewRequest("POST", r.web.URL+"/hook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body))
	for k, v := range hdr {
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

func TestSubscriptionHandshake(t *testing.T) {
	r := newRig(t, nil)
	get := func(q string) (int, string) {
		resp, err := http.Get(r.web.URL + "/hook?" + q)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("hub.mode=subscribe&hub.verify_token=" + verifyToken + "&hub.challenge=12345"); code != 200 || body != "12345" {
		t.Errorf("%d %q", code, body)
	}
	if code, _ := get("hub.mode=subscribe&hub.verify_token=wrong&hub.challenge=1"); code != 403 {
		t.Errorf("wrong token: %d", code)
	}
	if code, _ := get("hub.mode=subscribe&hub.challenge=1"); code != 403 {
		t.Errorf("no token: %d", code)
	}
}

func TestSignaturesAreChecked(t *testing.T) {
	r := newRig(t, nil)
	body := `{"items":[{"id":"1","chat":"c","text":"hi"}]}`
	if code := r.post(body, nil); code != 200 {
		t.Fatalf("a good request: %d", code)
	}
	r.next()
	for name, hdr := range map[string]map[string]string{
		"missing":     {"X-Hub-Signature-256": ""},
		"wrong":       {"X-Hub-Signature-256": "sha256=00"},
		"not hex":     {"X-Hub-Signature-256": "sha256=zz"},
		"other body":  {"X-Hub-Signature-256": sign(`{"items":[]}`)},
		"sha1 header": {"X-Hub-Signature-256": "sha1=" + strings.TrimPrefix(sign(body), "sha256=")},
	} {
		b := strings.Replace(body, `"1"`, `"2"`, 1)
		if code := r.post(b, hdr); code != 401 {
			t.Errorf("%s: %d", name, code)
		}
	}
	r.none()
	if code := r.post("not json", nil); code != 400 {
		t.Errorf("garbage: %d", code)
	}
	if code := r.post(`{"items":[],"pad":"`+strings.Repeat("x", 2<<20)+`"}`, nil); code != 413 {
		t.Errorf("oversized: %d", code)
	}
}

func TestBadSignaturesGetAnAddressTurnedAway(t *testing.T) {
	r := newRigLimit(t, nil, 5)
	var last int
	for i := 0; i < 8; i++ {
		last = r.post(`{}`, map[string]string{"X-Hub-Signature-256": "sha256=00"})
	}
	if last != 429 {
		t.Errorf("last = %d", last)
	}
}

func TestWebhookIsNotLimitedByTheSharedLimit(t *testing.T) {
	r := newRig(t, nil) // 2 requests per minute and address on other routes
	for i := 0; i < 10; i++ {
		if code := r.post(`{"items":[]}`, nil); code != 200 {
			t.Fatalf("request %d: %d", i, code)
		}
	}
}

func TestRedeliveryAndOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	r := newRig(t, func(in channel.Incoming) { mu.Lock(); order = append(order, in.Text); mu.Unlock() })
	var items []string
	for i := 0; i < 12; i++ {
		items = append(items, fmt.Sprintf(`{"id":"m%d","chat":"c","text":"t%02d"}`, i, i))
	}
	body := `{"items":[` + strings.Join(items, ",") + `]}`
	r.post(body, nil)
	for i := 0; i < 12; i++ {
		r.next()
	}
	r.post(body, nil) // the platform delivers it again
	r.none()
	mu.Lock()
	defer mu.Unlock()
	for i, s := range order {
		if s != fmt.Sprintf("t%02d", i) {
			t.Fatalf("order = %v", order)
		}
	}
}

func TestAnsweredBeforeHandled(t *testing.T) {
	release := make(chan struct{})
	r := newRig(t, func(channel.Incoming) { <-release })
	done := make(chan int, 1)
	go func() { done <- r.post(`{"items":[{"id":"1","chat":"c","text":"slow"}]}`, nil) }()
	select {
	case code := <-done:
		if code != 200 {
			t.Error(code)
		}
	case <-time.After(3 * time.Second):
		t.Error("the webhook waits for the model")
	}
	close(release)
	r.next()
}

func TestNotReadyBeforeRun(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := server.New(server.Options{Addr: ":0", Log: log})
	if _, err := NewReceiver(ReceiverConfig{Path: "/hook", AppSecret: appSecret, VerifyToken: verifyToken, Parse: parse}, srv, log); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body := `{"items":[]}`
	req, _ := http.NewRequest("POST", ts.URL+"/hook", strings.NewReader(body))
	req.Header.Set("X-Hub-Signature-256", sign(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("%d", resp.StatusCode)
	}
	for _, cfg := range []ReceiverConfig{{Path: "/h", VerifyToken: "v", Parse: parse}, {Path: "/h", AppSecret: "s", Parse: parse}, {Path: "h", AppSecret: "s", VerifyToken: "v", Parse: parse}} {
		if _, err := NewReceiver(cfg, server.New(server.Options{Addr: ":0", Log: log}), log); err == nil {
			t.Errorf("%+v accepted", cfg)
		}
	}
}

func TestClient(t *testing.T) {
	var gotAuth, gotQuery string
	status := 200
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		switch {
		case r.URL.Path == "/v1/file":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte("0123456789"))
		case status == 200:
			io.WriteString(w, `{"ok":true}`)
		case status == 500 && calls == 1:
			w.WriteHeader(500)
		case status == 500:
			io.WriteString(w, `{"ok":true}`)
		default:
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"Invalid OAuth access token.","code":190,"error_subcode":463}}`)
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL + "/v1/", Token: "TOKEN-SECRET"}
	var out struct{ OK bool }
	if err := c.Do(context.Background(), "POST", "/me/messages", map[string]string{"a": "b"}, &out); err != nil || !out.OK {
		t.Fatalf("%v %v", out, err)
	}
	if gotAuth != "Bearer TOKEN-SECRET" || gotQuery != "" {
		t.Errorf("the token belongs in the header only: %q %q", gotAuth, gotQuery)
	}
	status, calls = 401, 0
	err := c.Do(context.Background(), "POST", "/x", nil, nil)
	var ge *Error
	if !errors.As(err, &ge) || ge.Code != 190 || ge.Subcode != 463 || ge.Status != 401 || strings.Contains(err.Error(), "SECRET") || calls != 1 {
		t.Errorf("err = %v (%d calls)", err, calls)
	}
	status, calls = 500, 0
	if err := c.Do(context.Background(), "POST", "/x", nil, &out); err != nil || calls != 2 {
		t.Errorf("a server error is tried once more: %v (%d calls)", err, calls)
	}

	data, ctype, err := c.Download(context.Background(), srv.URL+"/v1/file", 100, false)
	if err != nil || string(data) != "0123456789" || ctype != "image/png" || gotAuth != "" {
		t.Errorf("public download: %q %q %v auth=%q", data, ctype, err, gotAuth)
	}
	if _, _, err = c.Download(context.Background(), srv.URL+"/v1/file", 100, true); err != nil || gotAuth != "Bearer TOKEN-SECRET" {
		t.Errorf("authorised download: %v auth=%q", err, gotAuth)
	}
	if _, _, err = c.Download(context.Background(), srv.URL+"/v1/file", 5, false); err != channel.ErrTooLarge {
		t.Errorf("over the limit: %v", err)
	}
}
