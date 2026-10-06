package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func prefixes(t *testing.T, s ...string) []netip.Prefix {
	t.Helper()
	p, err := ParseProxies(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func req(remote string, xff ...string) *http.Request {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = remote
	for _, v := range xff {
		r.Header.Add("X-Forwarded-For", v)
	}
	return r
}

func TestClientIP(t *testing.T) {
	proxy := prefixes(t, "10.0.0.0/8", "172.28.0.5")
	for name, tc := range map[string]struct {
		r       *http.Request
		trusted []netip.Prefix
		want    string
	}{
		"no proxies configured":      {req("203.0.113.9:1234", "1.2.3.4"), nil, "203.0.113.9"},
		"direct client ignores xff":  {req("203.0.113.9:1234", "1.2.3.4"), proxy, "203.0.113.9"},
		"trusted proxy, one hop":     {req("10.1.2.3:80", "198.51.100.7"), proxy, "198.51.100.7"},
		"forged left entries":        {req("10.1.2.3:80", "6.6.6.6, 198.51.100.7"), proxy, "198.51.100.7"},
		"chain through two proxies":  {req("10.1.2.3:80", "198.51.100.7, 10.9.9.9"), proxy, "198.51.100.7"},
		"multiple header lines":      {req("10.1.2.3:80", "6.6.6.6", "198.51.100.7"), proxy, "198.51.100.7"},
		"single-ip proxy":            {req("172.28.0.5:80", "198.51.100.7"), proxy, "198.51.100.7"},
		"proxy without header":       {req("10.1.2.3:80"), proxy, "10.1.2.3"},
		"garbage in the chain":       {req("10.1.2.3:80", "not-an-ip"), proxy, "10.1.2.3"},
		"all entries are proxies":    {req("10.1.2.3:80", "10.5.5.5"), proxy, "10.1.2.3"},
		"ipv6 client":                {req("10.1.2.3:80", "2001:db8::1"), proxy, "2001:db8::1"},
		"ipv4-mapped peer is mapped": {req("[::ffff:10.1.2.3]:80", "198.51.100.7"), proxy, "198.51.100.7"},
		"peer without port":          {req("203.0.113.9"), nil, "203.0.113.9"},
	} {
		if got := ClientIP(tc.r, tc.trusted); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

func TestParseProxies(t *testing.T) {
	p, err := ParseProxies([]string{" 10.0.0.0/8 ", "", "192.168.1.7", "::1", "172.16.5.9/12"})
	if err != nil || len(p) != 4 {
		t.Fatalf("p=%v err=%v", p, err)
	}
	if p[3].String() != "172.16.0.0/12" {
		t.Errorf("not masked: %v", p[3])
	}
	if _, err := ParseProxies([]string{"nonsense"}); err == nil {
		t.Error("garbage accepted")
	}
}

func TestHTTPSAndHostFromTrustedProxyOnly(t *testing.T) {
	s := New(Options{Addr: ":0", TrustedProxies: prefixes(t, "10.0.0.0/8"), Log: quiet()})
	r := req("10.0.0.2:1")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "bot.example.com, other")
	if !s.IsHTTPS(r) || s.Host(r) != "bot.example.com" {
		t.Errorf("trusted proxy headers ignored: https=%v host=%q", s.IsHTTPS(r), s.Host(r))
	}
	r = req("203.0.113.9:1")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "evil.example")
	if s.IsHTTPS(r) || s.Host(r) == "evil.example" {
		t.Error("headers from an untrusted peer were believed")
	}
}

func TestHealthzAndSecurityHeaders(t *testing.T) {
	s := New(Options{Addr: ":0", Version: "1.2", TrustedProxies: prefixes(t, "10.0.0.0/8"), Log: quiet()})
	h := s.Handler()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ok 1.2") {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("headers = %v", rec.Header())
	}
	if rec.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be sent over plain HTTP")
	}
	r := req("10.0.0.2:1")
	r.Header.Set("X-Forwarded-Proto", "https")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS missing behind an HTTPS proxy")
	}
}

func TestPerIPRateLimitSparesHealthz(t *testing.T) {
	s := New(Options{Addr: ":0", RatePerMinute: 3, Log: quiet()})
	s.Mux().HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	h := s.Handler()
	codes := func(ip string, n int, path string) (out []int) {
		for i := 0; i < n; i++ {
			r := httptest.NewRequest("GET", path, nil)
			r.RemoteAddr = ip + ":9"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			out = append(out, rec.Code)
			if rec.Code == 429 && rec.Header().Get("Retry-After") == "" {
				t.Error("429 without Retry-After")
			}
		}
		return
	}
	got := codes("198.51.100.1", 5, "/x")
	if got[2] != 204 || got[3] != 429 || got[4] != 429 {
		t.Errorf("codes = %v", got)
	}
	if other := codes("198.51.100.2", 1, "/x"); other[0] != 204 {
		t.Error("limit must be per IP")
	}
	for _, c := range codes("198.51.100.1", 10, "/healthz") {
		if c != 200 {
			t.Errorf("healthz must never be limited, got %d", c)
		}
	}
}

func TestPanicsAreContained(t *testing.T) {
	s := New(Options{Addr: ":0", Log: quiet()})
	s.Mux().HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "kaboom") {
		t.Errorf("code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestClientKeyGroupsIPv6ByNetwork(t *testing.T) {
	s := New(Options{Addr: ":0", Log: quiet()})
	key := func(remote string) string { return s.ClientKey(req(remote)) }
	if key("[2001:db8:1:2::1]:1") != key("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:1") {
		t.Error("two addresses of one /64 must share a key")
	}
	if key("[2001:db8:1:2::1]:1") == key("[2001:db8:1:3::1]:1") {
		t.Error("different /64 networks must not share a key")
	}
	if key("203.0.113.9:1") != "203.0.113.9" || key("203.0.113.9:1") == key("203.0.113.10:1") {
		t.Error("IPv4 addresses are keyed exactly")
	}
	if key("[::ffff:203.0.113.9]:1") != "203.0.113.9" {
		t.Error("IPv4-mapped IPv6 must be treated as IPv4")
	}

	lim := New(Options{Addr: ":0", RatePerMinute: 2, Log: quiet()})
	lim.Mux().HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	got := []int{}
	for _, remote := range []string{"[2001:db8:9::1]:1", "[2001:db8:9::2]:1", "[2001:db8:9::3]:1", "[2001:db8:9::4]:1"} {
		r := httptest.NewRequest("GET", "/x", nil)
		r.RemoteAddr = remote
		rec := httptest.NewRecorder()
		lim.Handler().ServeHTTP(rec, r)
		got = append(got, rec.Code)
	}
	if got[0] != 204 || got[1] != 204 || got[2] != 429 || got[3] != 429 {
		t.Errorf("rotating addresses inside one /64 dodged the limit: %v", got)
	}
}

func TestExemptPathsAreNotLimited(t *testing.T) {
	s := New(Options{Addr: ":0", RatePerMinute: 2, Log: quiet()})
	s.Mux().HandleFunc("POST /hook", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	s.Mux().HandleFunc("GET /x", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })
	s.ExemptFromRateLimit("/hook")
	h := s.Handler()
	do := func(method, path string) int {
		r := httptest.NewRequest(method, path, nil)
		r.RemoteAddr = "198.51.100.9:1"
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec.Code
	}
	for i := 0; i < 20; i++ {
		if c := do("POST", "/hook"); c != 204 {
			t.Fatalf("hook request %d: %d", i, c)
		}
	}
	// the exemption does not use up the address's allowance for other paths, nor extend to them
	do("GET", "/x")
	do("GET", "/x")
	if c := do("GET", "/x"); c != 429 {
		t.Errorf("other paths stay limited: %d", c)
	}
}
