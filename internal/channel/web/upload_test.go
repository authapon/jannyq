package web

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

type upload struct {
	name, mime string
	data       []byte
}

func multipartBody(t *testing.T, text string, files []upload, extra map[string]string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if text != "" {
		_ = mw.WriteField("text", text)
	}
	for k, v := range extra {
		_ = mw.WriteField(k, v)
	}
	for _, f := range files {
		hdr := textproto.MIMEHeader{}
		hdr.Set("Content-Disposition", `form-data; name="files"; filename="`+f.name+`"`)
		if f.mime != "" {
			hdr.Set("Content-Type", f.mime)
		}
		part, _ := mw.CreatePart(hdr)
		_, _ = part.Write(f.data)
	}
	_ = mw.Close()
	return mw.FormDataContentType(), &buf
}

func (h *harness) upload(text string, files []upload, hdr map[string]string) (int, map[string]any) {
	h.t.Helper()
	ct, body := multipartBody(h.t, text, files, nil)
	return h.postRaw(ct, body, hdr)
}

func (h *harness) postRaw(ct string, body *bytes.Buffer, hdr map[string]string) (int, map[string]any) {
	h.t.Helper()
	req, _ := http.NewRequest("POST", h.ts.URL+"/api/send", body)
	req.Header.Set("Content-Type", ct)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = jsonDecode(resp, &m)
	return resp.StatusCode, m
}

func attachHarness(t *testing.T, tweak func(*Config)) *harness {
	h := newHarness(t, func(c *Config) {
		c.Attachments, c.MaxUploadBytes, c.MaxFiles = true, 1000, 2
		if tweak != nil {
			tweak(c)
		}
	})
	h.start()
	return h
}

func TestUploadDeliversFiles(t *testing.T) {
	h := attachHarness(t, nil)
	code, m := h.upload("see these", []upload{{"a.txt", "text/plain", []byte("alpha")}, {"../../b.pdf", "application/pdf", []byte("%PDF-1.4 beta")}}, nil)
	if code != 202 {
		t.Fatalf("%d %v", code, m)
	}
	waitUntil(t, func() bool { return len(h.incoming()) == 1 })
	in := h.incoming()[0]
	if in.Text != "see these" || !in.Addressed || len(in.Attachments) != 2 {
		t.Fatalf("incoming = %+v", in)
	}
	if in.Attachments[0].Name != "a.txt" || in.Attachments[0].MIME != "text/plain" || in.Attachments[1].Name != "b.pdf" {
		t.Errorf("attachments = %+v", in.Attachments)
	}
	for i, want := range []string{"alpha", "%PDF-1.4 beta"} {
		data, err := in.Attachments[i].Fetch(context.Background(), 100)
		if err != nil || string(data) != want {
			t.Errorf("file %d: %q %v", i, data, err)
		}
	}
	if _, err := in.Attachments[0].Fetch(context.Background(), 2); err == nil {
		t.Error("Fetch ignores the limit")
	}
}

func TestUploadWithoutText(t *testing.T) {
	h := attachHarness(t, nil)
	if code, m := h.upload("", []upload{{"a.png", "image/png", []byte("x")}}, nil); code != 202 {
		t.Fatalf("%d %v", code, m)
	}
	waitUntil(t, func() bool { return len(h.incoming()) == 1 })
	if in := h.incoming()[0]; in.Text != "" || len(in.Attachments) != 1 {
		t.Errorf("%+v", in)
	}
	if code, _ := h.upload("", nil, nil); code != 400 {
		t.Errorf("an empty upload: %d", code)
	}
}

func TestUploadLimits(t *testing.T) {
	h := attachHarness(t, nil)
	big := bytes.Repeat([]byte("x"), 1001)
	if code, m := h.upload("x", []upload{{"big.txt", "", big}}, nil); code != 413 || !strings.Contains(m["error"].(string), "file too large") {
		t.Errorf("one file over the limit: %d %v", code, m)
	}
	three := []upload{{"1", "", []byte("a")}, {"2", "", []byte("b")}, {"3", "", []byte("c")}}
	if code, m := h.upload("x", three, nil); code != 413 || !strings.Contains(m["error"].(string), "too many files") {
		t.Errorf("three files: %d %v", code, m)
	}
	// a body far over the total limit is cut off, not buffered
	huge := bytes.Repeat([]byte("x"), 200_000)
	if code, _ := h.upload("x", []upload{{"1", "", huge}}, nil); code != 413 {
		t.Errorf("huge body: %d", code)
	}
	// text too long
	if code, _ := h.upload(strings.Repeat("a", 101), []upload{{"1", "", []byte("a")}}, nil); code != 413 {
		t.Errorf("long text: %d", code)
	}
	if len(h.incoming()) != 0 {
		t.Errorf("rejected uploads reached the bot: %d", len(h.incoming()))
	}
}

func TestUploadSecurityChecks(t *testing.T) {
	h := attachHarness(t, nil)
	if code, _ := h.upload("x", []upload{{"a", "", []byte("a")}}, map[string]string{"Origin": "https://evil.example"}); code != 403 {
		t.Errorf("cross-site upload: %d", code)
	}
	if code, _ := h.upload("x", []upload{{"a", "", []byte("a")}}, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Errorf("cross-site upload (fetch metadata): %d", code)
	}
	// no session
	fresh := newHarness(t, func(c *Config) { c.Attachments = true })
	if code, _ := fresh.upload("x", []upload{{"a", "", []byte("a")}}, nil); code != 401 {
		t.Errorf("no session: %d", code)
	}
	// malformed multipart
	ct, _ := multipartBody(t, "x", nil, nil)
	if code, _ := h.postRaw(ct, bytes.NewBufferString("not multipart at all"), nil); code != 400 {
		t.Errorf("malformed: %d", code)
	}
	if len(h.incoming()) != 0 || len(fresh.incoming()) != 0 {
		t.Error("refused uploads reached the bot")
	}
}

func TestUploadsAreRateLimitedLikeMessages(t *testing.T) {
	h := attachHarness(t, func(c *Config) { c.IPRate = 2 })
	for i, want := range []int{202, 202, 429} {
		if code, _ := h.upload("x", []upload{{"a", "", []byte("a")}}, nil); code != want {
			t.Errorf("upload %d: %d, want %d", i, code, want)
		}
	}
}

func TestUploadsDisabledByDefault(t *testing.T) {
	h := newHarness(t, nil)
	h.start()
	// without the feature a multipart body is just a bad request
	if code, _ := h.upload("x", []upload{{"a", "", []byte("a")}}, nil); code != 415 {
		t.Errorf("code = %d", code)
	}
	if _, m := h.json("GET", "/api/config", "", nil); m["attachments"] != false {
		t.Errorf("config = %v", m)
	}
}

func TestConfigAdvertisesLimits(t *testing.T) {
	h := attachHarness(t, nil)
	_, m := h.json("GET", "/api/config", "", nil)
	if m["attachments"] != true || m["maxFiles"] != float64(2) || m["maxFileBytes"] != float64(1000) {
		t.Errorf("config = %v", m)
	}
}

func TestHistoryShowsAttachmentNames(t *testing.T) {
	var hist fakeHistory
	h := attachHarness(t, func(c *Config) { c.History = &hist })
	id := strings.SplitN(h.cookieValue(), ".", 2)[0]
	hist.turns = map[string][]Turn{id: {{Role: "user", Text: "look", Attachments: []string{"a.pdf", "b.png"}}}}
	_, m := h.json("GET", "/api/history", "", nil)
	msgs := m["messages"].([]any)
	att := msgs[0].(map[string]any)["attachments"].([]any)
	if len(att) != 2 || att[0] != "a.pdf" {
		t.Errorf("history = %v", m)
	}
}

func TestSlowUploadsDoNotPileUp(t *testing.T) {
	h := attachHarness(t, nil)
	// fill every upload slot
	for i := 0; i < cap(h.ch.uploads); i++ {
		h.ch.uploads <- struct{}{}
	}
	code, _ := h.upload("x", []upload{{"a", "", []byte("a")}}, nil)
	for i := 0; i < cap(h.ch.uploads); i++ {
		<-h.ch.uploads
	}
	if code != 503 {
		t.Errorf("code = %d", code)
	}
	time.Sleep(10 * time.Millisecond)
}

func jsonDecode(resp *http.Response, v any) error {
	return json.NewDecoder(resp.Body).Decode(v)
}
