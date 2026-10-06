package meta

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/webhook"
)

// Host is the HTTP server a webhook is mounted on.
type Host interface {
	Mux() *http.ServeMux
	ClientKey(r *http.Request) string
	ExemptFromRateLimit(paths ...string)
}

// Item is one message found in a webhook payload.
type Item struct {
	// Key identifies the message, to ignore redelivered ones.
	Key string
	// Chat orders the messages: those of one chat are handled in the order they arrived.
	Chat string
	// Build creates the Incoming. It runs after the webhook has been answered,
	// so it may call the platform (for the sender's name, say).
	Build func(ctx context.Context) channel.Incoming
}

// ReceiverConfig configures a Receiver.
type ReceiverConfig struct {
	Name        string // for logs
	Path        string // webhook path
	AppSecret   string // signs the payloads (X-Hub-Signature-256)
	VerifyToken string // answers the subscription handshake
	// Parse finds the messages in a verified payload.
	Parse       func(body []byte) ([]Item, error)
	MaxInFlight int // default 64
	// BadPerMinute is how many refused requests an address may send (default 30).
	BadPerMinute int
}

// Receiver is the webhook half of a Meta channel: it answers the handshake,
// checks signatures, ignores redeliveries, answers at once and hands the
// messages on in order.
type Receiver struct {
	cfg     ReceiverConfig
	host    Host
	log     *slog.Logger
	orderer *channel.Orderer
	dedupe  *webhook.Dedupe
	bad     *webhook.Failures

	inFlight atomic.Int64
	mu       sync.Mutex
	sink     channel.Sink
	ctx      context.Context
	wg       sync.WaitGroup
}

// NewReceiver registers the webhook routes on host.
func NewReceiver(cfg ReceiverConfig, host Host, log *slog.Logger) (*Receiver, error) {
	if cfg.AppSecret == "" || cfg.VerifyToken == "" || cfg.Parse == nil {
		return nil, errors.New("meta: the app secret and the verify token are required")
	}
	if !strings.HasPrefix(cfg.Path, "/") {
		return nil, errors.New("meta: the webhook path must start with /")
	}
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 64
	}
	if cfg.BadPerMinute <= 0 {
		cfg.BadPerMinute = 30
	}
	if log == nil {
		log = slog.Default()
	}
	r := &Receiver{cfg: cfg, host: host, log: log, orderer: channel.NewOrderer(),
		dedupe: webhook.NewDedupe(15*time.Minute, 20000), bad: webhook.NewFailures(cfg.BadPerMinute, time.Minute)}
	host.Mux().Handle("GET "+cfg.Path, webhook.MetaChallenge(cfg.VerifyToken))
	host.Mux().HandleFunc("POST "+cfg.Path, r.handle)
	host.ExemptFromRateLimit(cfg.Path)
	return r, nil
}

// Run accepts messages until ctx ends and waits for those in progress.
func (r *Receiver) Run(ctx context.Context, sink channel.Sink) error {
	r.mu.Lock()
	r.sink, r.ctx = sink, ctx
	r.mu.Unlock()
	<-ctx.Done()
	r.wg.Wait()
	return nil
}

func (r *Receiver) handle(w http.ResponseWriter, req *http.Request) {
	key := r.host.ClientKey(req)
	if r.bad.Blocked(key) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	webhook.Signed(webhook.DefaultMaxBody, func(req *http.Request, body []byte) bool {
		ok := webhook.VerifyHex([]byte(r.cfg.AppSecret), body, req.Header.Get("X-Hub-Signature-256"))
		if !ok {
			r.bad.Add(key)
		}
		return ok
	}, r.accept).ServeHTTP(w, req)
}

func (r *Receiver) accept(w http.ResponseWriter, _ *http.Request, body []byte) {
	items, err := r.cfg.Parse(body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	sink, ctx := r.sink, r.ctx
	r.mu.Unlock()
	if sink == nil {
		http.Error(w, "starting up", http.StatusServiceUnavailable) // the platform delivers again later
		return
	}
	for _, it := range items {
		if r.dedupe.Seen(it.Key) {
			continue
		}
		if r.inFlight.Add(1) > int64(r.cfg.MaxInFlight) {
			r.inFlight.Add(-1)
			r.log.Warn("too many messages at once: one was dropped", "channel", r.cfg.Name)
			continue
		}
		wait, accepted := r.orderer.Enter(it.Chat) // in arrival order, before the goroutine starts
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			defer r.inFlight.Add(-1)
			defer accepted()
			if wait != nil {
				select {
				case <-wait:
				case <-ctx.Done():
					return
				}
			}
			in := it.Build(ctx)
			in.Accepted = accepted
			sink(ctx, in)
		}()
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "EVENT_RECEIVED")
}
