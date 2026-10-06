// Package webhook has the pieces every webhook-based channel needs: reading
// a bounded body, verifying the platform's signature in constant time,
// answering Meta's subscription handshake, and dropping redelivered events.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultMaxBody bounds webhook payloads; real events are a few kilobytes.
const DefaultMaxBody = 1 << 20

func mac(secret, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write(body)
	return m.Sum(nil)
}

// VerifyHex checks an HMAC-SHA256 signature sent as hex, with or without a
// "sha256=" prefix (Meta's X-Hub-Signature-256 header). An empty secret never verifies.
func VerifyHex(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	header = strings.TrimPrefix(strings.TrimSpace(header), "sha256=")
	got, err := hex.DecodeString(header)
	if err != nil {
		return false
	}
	return hmac.Equal(got, mac(secret, body))
}

// VerifyBase64 checks an HMAC-SHA256 signature sent as base64 (LINE's
// X-Line-Signature header). An empty secret never verifies.
func VerifyBase64(secret, body []byte, header string) bool {
	if len(secret) == 0 {
		return false
	}
	got, err := base64.StdEncoding.DecodeString(strings.TrimSpace(header))
	if err != nil {
		return false
	}
	return hmac.Equal(got, mac(secret, body))
}

// ErrTooLarge is returned by ReadBody for payloads over the limit.
var ErrTooLarge = errors.New("webhook: payload too large")

// ReadBody reads at most max bytes of the request body.
func ReadBody(w http.ResponseWriter, r *http.Request, max int64) ([]byte, error) {
	if max <= 0 {
		max = DefaultMaxBody
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return nil, ErrTooLarge
		}
		return nil, err
	}
	return body, nil
}

// Signed wraps a handler for POST webhooks: it reads the body (bounded),
// checks the signature with verify and only then calls next with the
// verified body. Bad signatures get 401 and no details. verify receives the
// raw body and the request, so it can pick the header and secret.
func Signed(maxBody int64, verify func(r *http.Request, body []byte) bool, next func(w http.ResponseWriter, r *http.Request, body []byte)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := ReadBody(w, r, maxBody)
		switch {
		case errors.Is(err, ErrTooLarge):
			http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
			return
		case err != nil:
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if !verify(r, body) {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		next(w, r, body)
	})
}

// MetaChallenge answers the GET handshake Meta (Messenger, WhatsApp,
// Instagram) uses to verify a webhook URL: when hub.verify_token matches, the
// hub.challenge value is echoed back.
func MetaChallenge(verifyToken string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		token := q.Get("hub.verify_token")
		if verifyToken == "" || q.Get("hub.mode") != "subscribe" ||
			subtle.ConstantTimeCompare([]byte(token), []byte(verifyToken)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, q.Get("hub.challenge"))
	})
}

// Dedupe remembers recently seen event IDs. Platforms redeliver webhooks
// that were not acknowledged quickly, and replayed requests must not make the
// bot answer twice.
type Dedupe struct {
	ttl time.Duration
	max int
	now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// NewDedupe remembers up to max keys for ttl each.
func NewDedupe(ttl time.Duration, max int) *Dedupe {
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	if max <= 0 {
		max = 10000
	}
	return &Dedupe{ttl: ttl, max: max, now: time.Now, seen: map[string]time.Time{}}
}

// Seen records key and reports whether it was already recorded within the ttl.
// Empty keys are never considered duplicates.
func (d *Dedupe) Seen(key string) bool {
	if key == "" {
		return false
	}
	now := d.now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.seen[key]; ok && now.Sub(t) < d.ttl {
		return true
	}
	if len(d.seen) >= d.max {
		for k, t := range d.seen { // drop expired entries first
			if now.Sub(t) >= d.ttl {
				delete(d.seen, k)
			}
		}
		for k := range d.seen { // still full: drop arbitrary entries
			if len(d.seen) < d.max {
				break
			}
			delete(d.seen, k)
		}
	}
	d.seen[key] = now
	return false
}
