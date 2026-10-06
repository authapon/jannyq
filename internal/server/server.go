// Package server hosts the bot's HTTP endpoints: the health check, the web
// chat and the webhooks of messaging platforms. All routes share one set of
// protections: panic recovery, security headers, per-client rate limiting and
// trustworthy client IP detection behind a reverse proxy.
package server

import (
	"context"
	"errors"
	"fmt"
	"github.com/authapon/jannyq/internal/metrics"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/authapon/jannyq/internal/ratelimit"
)

// Options configures a Server.
type Options struct {
	Addr    string
	Version string
	// TrustedProxies are the reverse proxies allowed to tell us the client
	// address through X-Forwarded-For. Without any, the TCP peer is the client.
	TrustedProxies []netip.Prefix
	// RatePerMinute limits requests per client IP across all routes except
	// /healthz; 0 or less disables the limit.
	RatePerMinute int
	Log           *slog.Logger
	// Requests, when set, counts requests by status class ("2xx", ...).
	Requests *metrics.Counter
}

// Server is an HTTP server with a mux that channels add routes to.
type Server struct {
	opts    Options
	mux     *http.ServeMux
	limiter *ratelimit.Limiter
	log     *slog.Logger

	exemptMu sync.RWMutex
	exempt   map[string]bool
}

// New creates a server listening on opts.Addr (e.g. ":8080").
func New(opts Options) *Server {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok %s\n", opts.Version)
	})
	return &Server{opts: opts, mux: mux, limiter: ratelimit.New(opts.RatePerMinute, time.Minute), log: opts.Log}
}

// ExemptFromRateLimit takes paths out of the per-address limit. It is for
// webhooks that platforms call from a few shared addresses: they must check
// their own signatures and limit what they refuse.
func (s *Server) ExemptFromRateLimit(paths ...string) {
	s.exemptMu.Lock()
	defer s.exemptMu.Unlock()
	if s.exempt == nil {
		s.exempt = map[string]bool{}
	}
	for _, p := range paths {
		s.exempt[p] = true
	}
}

func (s *Server) isExempt(path string) bool {
	s.exemptMu.RLock()
	defer s.exemptMu.RUnlock()
	return s.exempt[path]
}

// Mux returns the router for registering routes.
func (s *Server) Mux() *http.ServeMux { return s.mux }

// Handler returns the mux wrapped in the shared middleware.
func (s *Server) Handler() http.Handler {
	var h http.Handler = s.mux
	h = s.rateLimit(h)
	h = s.securityHeaders(h)
	h = s.accessLog(h)
	h = s.recoverPanics(h)
	h = s.count(h) // outside the recovery, so that a panic is counted as the 500 it became
	return h
}

// count counts requests by the class of their status.
func (s *Server) count(next http.Handler) http.Handler {
	if s.opts.Requests == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.opts.Requests.Inc(strconv.Itoa(sw.status/100) + "xx")
	})
}

// Run serves until ctx is cancelled, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // streaming handlers extend it per write
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	s.log.Info("http server listening", "addr", ln.Addr().String())
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.Canceled) {
			return err
		}
		return nil
	}
}

// ClientIP returns the address of the client that made the request. The
// X-Forwarded-For header is believed only when the TCP peer is a trusted
// proxy, and then the right-most address that is not itself a trusted proxy
// is used: entries further left can be forged by the client.
func (s *Server) ClientIP(r *http.Request) string {
	return ClientIP(r, s.opts.TrustedProxies)
}

// ClientIP is the function behind Server.ClientIP.
func ClientIP(r *http.Request, trusted []netip.Prefix) string {
	peer := peerAddr(r.RemoteAddr)
	if !peer.IsValid() {
		return r.RemoteAddr
	}
	if !isTrusted(peer, trusted) {
		return peer.String()
	}
	ips := r.Header.Values("X-Forwarded-For")
	if len(ips) == 0 {
		return peer.String()
	}
	parts := strings.Split(strings.Join(ips, ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			return peer.String() // garbage from the proxy chain: do not guess
		}
		addr = addr.Unmap()
		if !isTrusted(addr, trusted) {
			return addr.String()
		}
	}
	return peer.String()
}

// ClientKey is ClientIP prepared for rate limiting: IPv6 addresses are reduced
// to their /64 network, because one subscriber typically controls a whole /64
// and could otherwise take a fresh address for every request.
func (s *Server) ClientKey(r *http.Request) string {
	return limitKey(s.ClientIP(r))
}

func limitKey(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	a = a.Unmap()
	if a.Is6() {
		return netip.PrefixFrom(a, 64).Masked().String()
	}
	return a.String()
}

// IsHTTPS reports whether the client connected over TLS, directly or through
// a trusted proxy that says so.
func (s *Server) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	peer := peerAddr(r.RemoteAddr)
	return peer.IsValid() && isTrusted(peer, s.opts.TrustedProxies) &&
		strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// Host returns the host the client asked for, honouring X-Forwarded-Host from
// trusted proxies.
func (s *Server) Host(r *http.Request) string {
	peer := peerAddr(r.RemoteAddr)
	if peer.IsValid() && isTrusted(peer, s.opts.TrustedProxies) {
		if h := r.Header.Get("X-Forwarded-Host"); h != "" {
			return strings.TrimSpace(strings.Split(h, ",")[0])
		}
	}
	return r.Host
}

func peerAddr(remote string) netip.Addr {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func isTrusted(a netip.Addr, trusted []netip.Prefix) bool {
	for _, p := range trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ParseProxies parses CIDR ranges or single addresses.
func ParseProxies(list []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, s := range list {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if p, err := netip.ParsePrefix(s); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil {
			return nil, fmt.Errorf("%q is not an IP address or CIDR range", s)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

// --- middleware ---

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the real writer (flushing, deadlines).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil && v != http.ErrAbortHandler {
				s.log.Error("panic in http handler", "path", r.URL.Path, "panic", fmt.Sprint(v))
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// accessLog logs requests at debug level. The query string is left out on
// purpose: it can carry tokens.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.log.Enabled(r.Context(), slog.LevelDebug) {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		s.log.Debug("http request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
			"duration", time.Since(start).Round(time.Millisecond), "ip", s.ClientIP(r))
	})
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if s.IsHTTPS(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && !s.isExempt(r.URL.Path) && !s.limiter.Allow(s.ClientKey(r)) {
			TooManyRequests(w, time.Minute)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// TooManyRequests answers 429 with a Retry-After hint.
func TooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())))
	http.Error(w, "too many requests", http.StatusTooManyRequests)
}
