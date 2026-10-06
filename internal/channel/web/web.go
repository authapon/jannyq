// Package web implements the web chat: a small page served by the bot's own
// HTTP server. Browsers talk to it with plain HTTP (JSON to send, Server-Sent
// Events to receive), so no extra infrastructure is needed.
//
// Visitors are identified by a signed cookie; an optional access code gates
// who may start a chat. Every endpoint is protected against cross-site
// requests and rate limited per client address.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/ratelimit"
)

//go:embed static/*
var staticFS embed.FS

// Turn is one message of the conversation shown after a page reload.
type Turn struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// History provides the stored conversation of a chat.
type History interface {
	Recent(ctx context.Context, chatID string, n int) ([]Turn, error)
}

// Host is the HTTP server the chat is mounted on.
type Host interface {
	Mux() *http.ServeMux
	ClientIP(r *http.Request) string
	// ClientKey is the address in the form used for limits (IPv6 by /64).
	ClientKey(r *http.Request) string
	IsHTTPS(r *http.Request) bool
	Host(r *http.Request) string
}

// Config configures the web chat.
type Config struct {
	BasePath   string // where the chat lives, "/" or e.g. "/chat/"
	Title      string
	Lang       string
	AccessCode string // empty: anyone may chat
	Secret     []byte // signs session cookies
	MaxMessage int    // characters per message

	IPRate             int // messages per client address per minute
	NewSessionsPerHour int // new anonymous sessions per client address
	SecureCookies      string
	AllowedOrigins     []string
	Strings            map[string]string // UI texts
	History            History
	// MaxInFlight bounds messages being processed at once; 0 selects 64.
	MaxInFlight int
}

const (
	maxBodyBytes   = 64 << 10
	historyTurns   = 50
	sseKeepAlive   = 20 * time.Second
	sseWriteWindow = 15 * time.Second
)

// Channel is the web chat channel.
type Channel struct {
	cfg    Config
	host   Host
	log    *slog.Logger
	signer *signer
	hub    *hub

	sendLimiter    *ratelimit.Limiter
	loginLimiter   *ratelimit.Limiter
	sessionLimiter *ratelimit.Limiter
	inFlight       atomic.Int64

	mu   sync.Mutex
	sink channel.Sink
	ctx  context.Context
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// New creates the channel and registers its routes on host.
func New(cfg Config, host Host, log *slog.Logger) (*Channel, error) {
	if len(cfg.Secret) < 16 {
		return nil, errors.New("web: the cookie secret must be at least 16 bytes")
	}
	if cfg.BasePath == "" {
		cfg.BasePath = "/"
	}
	if !strings.HasPrefix(cfg.BasePath, "/") {
		cfg.BasePath = "/" + cfg.BasePath
	}
	if !strings.HasSuffix(cfg.BasePath, "/") {
		cfg.BasePath += "/"
	}
	if cfg.MaxMessage <= 0 {
		cfg.MaxMessage = 4000
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 64
	}
	if cfg.Title == "" {
		cfg.Title = "Chat"
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Channel{
		cfg: cfg, host: host, log: log.With("channel", "web"),
		signer:         newSigner(cfg.Secret, cfg.AccessCode),
		hub:            newHub(),
		sendLimiter:    ratelimit.New(cfg.IPRate, time.Minute),
		loginLimiter:   ratelimit.New(10, time.Minute),
		sessionLimiter: ratelimit.New(cfg.NewSessionsPerHour, time.Hour),
		done:           make(chan struct{}),
	}
	c.routes()
	return c, nil
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "web" }

// Run implements channel.Channel: it accepts messages until ctx ends. The
// routes were registered by New; HTTP serving belongs to the Host.
func (c *Channel) Run(ctx context.Context, sink channel.Sink) error {
	c.mu.Lock()
	c.sink, c.ctx = sink, ctx
	c.mu.Unlock()
	<-ctx.Done()
	c.once.Do(func() { close(c.done) }) // ends open event streams
	c.wg.Wait()
	return nil
}

// routes registers the endpoints under the base path.
func (c *Channel) routes() {
	mux, base := c.host.Mux(), c.cfg.BasePath
	root := base + "{$}"
	if base == "/" {
		root = "GET /{$}"
	} else {
		root = "GET " + root
		mux.HandleFunc("GET "+strings.TrimSuffix(base, "/"), func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, base, http.StatusMovedPermanently)
		})
	}
	mux.HandleFunc(root, c.handlePage("static/index.html", "text/html; charset=utf-8"))
	mux.HandleFunc("GET "+base+"app.js", c.handlePage("static/app.js", "text/javascript; charset=utf-8"))
	mux.HandleFunc("GET "+base+"app.css", c.handlePage("static/app.css", "text/css; charset=utf-8"))
	mux.HandleFunc("GET "+base+"api/config", c.handleConfig)
	mux.HandleFunc("POST "+base+"api/login", c.handleLogin)
	mux.HandleFunc("GET "+base+"api/history", c.handleHistory)
	mux.HandleFunc("GET "+base+"api/events", c.handleEvents)
	mux.HandleFunc("POST "+base+"api/send", c.handleSend)
}

// --- responses ---

type apiError struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) { writeJSON(w, code, apiError{msg}) }

// pageHeaders locks the page down: scripts and styles only from this origin.
func pageHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; "+
		"img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	h.Set("Cache-Control", "no-cache")
}

func (c *Channel) handlePage(file, contentType string) http.HandlerFunc {
	data, err := staticFS.ReadFile(file)
	if err != nil {
		panic(err) // the file is embedded at build time
	}
	return func(w http.ResponseWriter, r *http.Request) {
		pageHeaders(w)
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(data)
	}
}

// --- sessions ---

// session returns the chat id of the request's cookie.
func (c *Channel) session(r *http.Request) (string, bool) {
	ck, err := r.Cookie(cookieName)
	if err != nil {
		return "", false
	}
	id := c.signer.verify(ck.Value)
	return id, id != ""
}

func (c *Channel) setCookie(w http.ResponseWriter, r *http.Request, value string) {
	secure := false
	switch c.cfg.SecureCookies {
	case "on":
		secure = true
	case "off":
	default:
		secure = c.host.IsHTTPS(r)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: value, Path: c.cfg.BasePath,
		MaxAge: 30 * 24 * 3600, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
}

func (c *Channel) needsCode() bool { return c.cfg.AccessCode != "" }

func (c *Channel) handleConfig(w http.ResponseWriter, r *http.Request) {
	_, authed := c.session(r)
	if !authed && !c.needsCode() {
		if !c.sessionLimiter.Allow(c.host.ClientKey(r)) {
			fail(w, http.StatusTooManyRequests, "too many new chats from this address")
			return
		}
		value, _ := c.signer.issue()
		c.setCookie(w, r, value)
		authed = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"title":      c.cfg.Title,
		"lang":       c.cfg.Lang,
		"maxMessage": c.cfg.MaxMessage,
		"needsCode":  c.needsCode(),
		"authed":     authed,
		"strings":    c.cfg.Strings,
	})
}

func (c *Channel) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !c.sameOrigin(r) {
		fail(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	if !c.needsCode() {
		fail(w, http.StatusNotFound, "no access code is configured")
		return
	}
	ip := c.host.ClientKey(r)
	if !c.loginLimiter.Allow(ip) {
		fail(w, http.StatusTooManyRequests, "too many attempts")
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if subtle.ConstantTimeCompare([]byte(req.Code), []byte(c.cfg.AccessCode)) != 1 {
		fail(w, http.StatusForbidden, "wrong code")
		return
	}
	if !c.sessionLimiter.Allow(ip) {
		fail(w, http.StatusTooManyRequests, "too many new chats from this address")
		return
	}
	value, _ := c.signer.issue()
	c.setCookie(w, r, value)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- request checks ---

// sameOrigin refuses cross-site browser requests. Browsers state where a
// request comes from in Origin and Sec-Fetch-Site; clients that send neither
// (curl, scripts) are not subject to cross-site attacks.
func (c *Channel) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, c.host.Host(r)) {
		return true
	}
	for _, allowed := range c.cfg.AllowedOrigins {
		if strings.EqualFold(strings.TrimRight(allowed, "/"), origin) {
			return true
		}
	}
	return false
}

// readJSON decodes a small JSON body, answering the error itself on failure.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); mt != "application/json" {
		fail(w, http.StatusUnsupportedMediaType, "content type must be application/json")
		return false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			fail(w, http.StatusBadRequest, "unreadable request")
		}
		return false
	}
	if err := json.Unmarshal(body, v); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return false
	}
	return true
}

// --- API ---

func (c *Channel) handleHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := c.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "no session")
		return
	}
	turns := []Turn{}
	if c.cfg.History != nil {
		got, err := c.cfg.History.Recent(r.Context(), id, historyTurns)
		if err != nil {
			c.log.Error("reading history failed", "err", err)
			fail(w, http.StatusInternalServerError, "history unavailable")
			return
		}
		if got != nil {
			turns = got
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": turns})
}

func (c *Channel) handleSend(w http.ResponseWriter, r *http.Request) {
	if !c.sameOrigin(r) {
		fail(w, http.StatusForbidden, "cross-site request refused")
		return
	}
	id, ok := c.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "no session")
		return
	}
	if !c.sendLimiter.Allow(c.host.ClientKey(r)) {
		w.Header().Set("Retry-After", "60")
		fail(w, http.StatusTooManyRequests, "too many messages")
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	text := strings.TrimSpace(req.Text)
	switch {
	case text == "" || !utf8.ValidString(text):
		fail(w, http.StatusBadRequest, "empty message")
		return
	case utf8.RuneCountInString(text) > c.cfg.MaxMessage:
		fail(w, http.StatusRequestEntityTooLarge, "message too long")
		return
	}

	c.mu.Lock()
	sink, ctx := c.sink, c.ctx
	c.mu.Unlock()
	if sink == nil {
		fail(w, http.StatusServiceUnavailable, "starting up")
		return
	}
	if c.inFlight.Add(1) > int64(c.cfg.MaxInFlight) {
		c.inFlight.Add(-1)
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "busy")
		return
	}
	origin := c.host.ClientKey(r)
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		defer c.inFlight.Add(-1)
		sink(ctx, channel.Incoming{
			Channel:   "web",
			ChatID:    id,
			UserID:    id,
			UserName:  "web user",
			Text:      text,
			Addressed: true,
			Origin:    origin,
			Responder: &responder{c: c, id: id},
		})
	}()
	writeJSON(w, http.StatusAccepted, map[string]bool{"accepted": true})
}

func (c *Channel) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := c.session(r)
	if !ok {
		fail(w, http.StatusUnauthorized, "no session")
		return
	}
	var lastID int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		lastID, _ = strconv.ParseInt(v, 10, 64)
	}
	sub, replay, cancel, err := c.hub.subscribe(id, c.host.ClientKey(r), lastID)
	if err != nil {
		w.Header().Set("Retry-After", "10")
		fail(w, http.StatusTooManyRequests, "too many open streams")
		return
	}
	defer cancel()

	rc := http.NewResponseController(w)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("X-Accel-Buffering", "no") // do not let proxies buffer the stream
	w.WriteHeader(http.StatusOK)

	write := func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteWindow))
		if _, err := io.WriteString(w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write("retry: 3000\n: connected\n\n") {
		return
	}
	for _, ev := range replay {
		if !write(formatEvent(ev)) {
			return
		}
	}
	tick := time.NewTicker(sseKeepAlive)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-sub.ch:
			if !ok { // dropped for being too slow: the browser reconnects and resumes
				return
			}
			if !write(formatEvent(ev)) {
				return
			}
		case <-tick.C:
			if !write(": ping\n\n") {
				return
			}
		case <-r.Context().Done():
			return
		case <-c.done:
			return
		}
	}
}

func formatEvent(ev event) string {
	var sb strings.Builder
	if ev.ID > 0 {
		fmt.Fprintf(&sb, "id: %d\n", ev.ID)
	}
	fmt.Fprintf(&sb, "event: %s\ndata: %s\n\n", ev.Type, ev.Data)
	return sb.String()
}

// responder delivers replies to the chat's open streams.
type responder struct {
	c  *Channel
	id string
}

func (r *responder) Send(_ context.Context, text string) error {
	r.c.hub.publish(r.id, "message", map[string]string{"text": text})
	return nil
}

func (r *responder) Typing(context.Context) error {
	r.c.hub.publish(r.id, "typing", map[string]bool{"typing": true})
	return nil
}
