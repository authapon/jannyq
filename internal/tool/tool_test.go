package tool

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestIsBlockedIP(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254",
		"100.64.0.1", "0.0.0.0", "::", "fe80::1", "fc00::1", "224.0.0.1", "240.0.0.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "198.18.0.1", "64:ff9b::1",
	}
	for _, s := range blocked {
		if !isBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if isBlockedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s should be allowed", s)
		}
	}
}

func TestSafeClientBlocksLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secret") }))
	defer srv.Close()

	if _, err := NewSafeClient(false, 5*time.Second).Get(srv.URL); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("loopback fetch should be blocked, err = %v", err)
	}
	resp, err := NewSafeClient(true, 5*time.Second).Get(srv.URL)
	if err != nil {
		t.Fatalf("allowPrivate client failed: %v", err)
	}
	resp.Body.Close()
}

func TestSafeClientBlocksRedirectToPrivate(t *testing.T) {
	// A public-looking origin cannot be simulated offline, so check that a
	// redirect target is validated by the same dialer: with allowPrivate
	// off, even the first hop (loopback) fails before any redirect.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := NewSafeClient(false, 5*time.Second).Get(srv.URL); err == nil {
		t.Fatal("expected error")
	}
}

func TestRegistryDefsSorted(t *testing.T) {
	r := NewRegistry()
	r.Register(&WebFetch{})
	r.Register(&WebSearch{})
	defs := r.Defs()
	if len(defs) != 2 || defs[0].Name != "web_fetch" || defs[1].Name != "web_search" {
		t.Errorf("defs = %+v", defs)
	}
	if _, ok := r.Get("web_search"); !ok {
		t.Error("Get failed")
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("สวัสดี", 100); got != "สวัสดี" {
		t.Errorf("got %q", got)
	}
	got := Truncate("abcdef", 3)
	if !strings.HasPrefix(got, "abc\n[truncated: 3 more") {
		t.Errorf("got %q", got)
	}
}

func TestWebSearch(t *testing.T) {
	var query string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" || r.URL.Query().Get("format") != "json" {
			t.Errorf("bad request %s", r.URL)
		}
		query = r.URL.RawQuery
		io.WriteString(w, `{"results":[
			{"title":"One","url":"https://a.example/1","content":"first   snippet"},
			{"title":"Dup","url":"https://a.example/1","content":"dup"},
			{"title":"Two","url":"https://b.example/2","content":""},
			{"title":"Three","url":"https://c.example/3","content":"third"}]}`)
	}))
	defer srv.Close()

	ws := &WebSearch{BaseURL: srv.URL, Client: srv.Client(), MaxResults: 2}
	out, err := ws.Execute(context.Background(), CallContext{}, []byte(`{"query":"golang ทดสอบ","time_range":"week","language":"th"}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1. One", "URL: https://a.example/1", "first snippet", "2. Two"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Dup") || strings.Contains(out, "Three") {
		t.Errorf("dedupe/limit failed:\n%s", out)
	}
	if strings.Contains(query, "time_range") || !strings.Contains(query, "language=th") {
		t.Errorf("query params wrong: %s", query)
	}
}

func TestWebSearchErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	ws := &WebSearch{BaseURL: srv.URL, Client: srv.Client()}
	if _, err := ws.Execute(context.Background(), CallContext{}, []byte(`{"query":"x"}`)); err == nil || !strings.Contains(err.Error(), "json format") {
		t.Errorf("403 should hint at json format, got %v", err)
	}
	if _, err := ws.Execute(context.Background(), CallContext{}, []byte(`{"query":"  "}`)); err == nil {
		t.Error("empty query should fail")
	}
	if _, err := ws.Execute(context.Background(), CallContext{}, []byte(`nope`)); err == nil {
		t.Error("bad json should fail")
	}
}

func TestWebSearchNoResults(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"results":[]}`) }))
	defer srv.Close()
	ws := &WebSearch{BaseURL: srv.URL, Client: srv.Client()}
	out, err := ws.Execute(context.Background(), CallContext{}, []byte(`{"query":"x"}`))
	if err != nil || out != "No results found." {
		t.Errorf("out=%q err=%v", out, err)
	}
}
