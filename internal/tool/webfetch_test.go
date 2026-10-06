package tool

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/charmap"
	"golang.org/x/text/encoding/unicode"
)

const samplePage = `<!doctype html><html><head><title> My  Page </title>
<style>body{color:red}</style><script>var secret = 1;</script></head>
<body>
<nav><a href="/home">Home</a> NAVTEXT</nav>
<main>
<h1>Main Heading</h1>
<p>First paragraph with <b>bold</b> and a <a href="/rel/link">relative link</a> and
<a href="#top">anchor</a>.</p>
<ul><li>one</li><li>two</li></ul>
<pre>line1
  line2</pre>
<p>` + "Lorem ipsum dolor sit amet. " + `Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet. Lorem ipsum dolor sit amet.</p>
</main>
<footer>FOOTERTEXT</footer>
</body></html>`

func newFetch(srv *httptest.Server) *WebFetch {
	return &WebFetch{Client: NewSafeClient(true, 5*time.Second), MaxBytes: 1 << 20, MaxChars: 12000}
}

func fetch(t *testing.T, f *WebFetch, args string) (string, error) {
	t.Helper()
	return f.Execute(context.Background(), CallContext{}, []byte(args))
}

func TestWebFetchHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, samplePage)
	}))
	defer srv.Close()
	out, err := fetch(t, newFetch(srv), `{"url":"`+srv.URL+`/page"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Title: My Page", "# Main Heading", "First paragraph with bold and a [relative link](" + srv.URL + "/rel/link) and anchor.",
		"- one\n- two", "line1\n  line2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	for _, bad := range []string{"NAVTEXT", "FOOTERTEXT", "secret", "color:red"} {
		if strings.Contains(out, bad) {
			t.Errorf("should not contain %q:\n%s", bad, out)
		}
	}
}

func TestWebFetchThaiLegacyCharset(t *testing.T) {
	enc, _ := charmap.Windows874.NewEncoder().String("<html><head><title>ทดสอบ</title></head><body><p>สวัสดีชาวโลก</p></body></html>")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=windows-874")
		io.WriteString(w, enc)
	}))
	defer srv.Close()
	out, err := fetch(t, newFetch(srv), `{"url":"`+srv.URL+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Title: ทดสอบ") || !strings.Contains(out, "สวัสดีชาวโลก") {
		t.Errorf("thai not decoded:\n%s", out)
	}
}

func TestWebFetchUTF16(t *testing.T) {
	var buf bytes.Buffer
	w := unicode.UTF16(unicode.LittleEndian, unicode.UseBOM).NewEncoder().Writer(&buf)
	io.WriteString(w, "<html><body><p>hello utf16</p></body></html>")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write(buf.Bytes())
	}))
	defer srv.Close()
	out, err := fetch(t, newFetch(srv), `{"url":"`+srv.URL+`"}`)
	if err != nil || !strings.Contains(out, "hello utf16") {
		t.Errorf("out=%q err=%v", out, err)
	}
}

func TestWebFetchPaging(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, strings.Repeat("ก", 100))
	}))
	defer srv.Close()
	f := newFetch(srv)
	out, err := fetch(t, f, `{"url":"`+srv.URL+`","max_chars":40}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, strings.Repeat("ก", 40)) || strings.Contains(out, strings.Repeat("ก", 41)) ||
		!strings.Contains(out, "Showing characters 0-40 of 100") || !strings.Contains(out, "offset=40") {
		t.Errorf("first page wrong:\n%s", out)
	}
	out, err = fetch(t, f, `{"url":"`+srv.URL+`","max_chars":40,"offset":80}`)
	if err != nil || !strings.Contains(out, strings.Repeat("ก", 20)) || strings.Contains(out, "Showing") {
		t.Errorf("last page wrong: %v\n%s", err, out)
	}
	if _, err := fetch(t, f, `{"url":"`+srv.URL+`","offset":500}`); err == nil {
		t.Error("offset beyond end should fail")
	}
}

func TestWebFetchRejections(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pdf":
			w.Header().Set("Content-Type", "application/pdf")
			io.WriteString(w, "%PDF-1.4")
		case "/404":
			http.NotFound(w, r)
		case "/big":
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, strings.Repeat("x", 5000))
		}
	}))
	defer srv.Close()
	f := newFetch(srv)
	for _, tc := range []struct{ args, want string }{
		{`{"url":"` + srv.URL + `/pdf"}`, "unsupported content type"},
		{`{"url":"` + srv.URL + `/404"}`, "HTTP 404"},
		{`{"url":"ftp://example.com/x"}`, "http or https"},
		{`{"url":"not a url"}`, "http or https"},
		{`{"url":""}`, "http or https"},
		{`garbage`, "invalid arguments"},
	} {
		if _, err := fetch(t, f, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.args, err, tc.want)
		}
	}
	// the download size limit truncates instead of failing
	f.MaxBytes = 1000
	out, err := fetch(t, f, `{"url":"`+srv.URL+`/big","max_chars":5000}`)
	if err != nil || strings.Count(out, "x") != 1000 {
		t.Errorf("max bytes: err=%v xs=%d", err, strings.Count(out, "x"))
	}
}

func TestWebFetchBlocksPrivateByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "hi") }))
	defer srv.Close()
	f := &WebFetch{Client: NewSafeClient(false, 5*time.Second)}
	if _, err := fetch(t, f, `{"url":"`+srv.URL+`"}`); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("err = %v", err)
	}
}

func TestHTMLToTextFallsBackToBodyWhenMainIsTiny(t *testing.T) {
	base, _ := url.Parse("https://x.example/")
	_, text, err := htmlToText(strings.NewReader(`<body><main>tiny</main><p>the rest of the page has real content</p></body>`), base)
	if err != nil || !strings.Contains(text, "real content") {
		t.Errorf("text=%q err=%v", text, err)
	}
}
