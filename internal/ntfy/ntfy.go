// Package ntfy sends push notifications through an ntfy server (ntfy.sh or
// one of your own): the message goes to a topic, and whoever subscribes to the
// topic in the ntfy app on a phone gets it.
package ntfy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Client publishes to one ntfy server. The address is the operator's choice;
// users only name topics, so they cannot make the bot call another address.
type Client struct {
	// BaseURL is the server, e.g. https://ntfy.sh.
	BaseURL string
	// Token authorises the bot: an access token ("tk_…") is sent as a bearer
	// token, "user:password" as basic authentication. Empty sends none.
	Token string
	// TopicPrefix is put in front of every topic, so that a token allowed to
	// write only to "prefix*" cannot be used to write to anybody else's topic.
	TopicPrefix string
	HTTP        *http.Client
	// Log, if set, receives the details of failed requests at debug level.
	Log *slog.Logger
	// MaxBytes is the longest message sent (default 3900; ntfy refuses more
	// than 4096 by default). Longer text is cut at a character boundary.
	MaxBytes int
}

// Message is a notification.
type Message struct {
	Topic    string // as the user named it; the prefix is added when sending
	Title    string
	Body     string
	Priority int      // 1 (min) to 5 (urgent); 0 means the default, 3
	Tags     []string // emoji short codes such as "alarm_clock"
}

var topicRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// backoff is the pause before the first retry (a variable so that tests can shorten it).
var backoff = 500 * time.Millisecond

const (
	maxTopic     = 64
	defaultBytes = 3900
	maxTitle     = 200
)

// Topic checks a topic name a user gave and returns the topic to publish to,
// with the prefix. A name that already starts with the prefix is not prefixed twice.
func (c *Client) Topic(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("the ntfy topic is empty")
	}
	if !topicRe.MatchString(name) {
		return "", fmt.Errorf("%q is not a valid ntfy topic: use only letters, digits, - and _ (no spaces)", name)
	}
	full := name
	if c.TopicPrefix != "" && !strings.HasPrefix(name, c.TopicPrefix) {
		full = c.TopicPrefix + name
	}
	if len(full) > maxTopic {
		return "", fmt.Errorf("the ntfy topic is too long (at most %d characters, including the prefix %q)", maxTopic, c.TopicPrefix)
	}
	return full, nil
}

// Short returns a few characters that identify a topic in a log without
// writing the topic down (a topic is a secret of sorts: anybody who knows it
// can read it on a public server).
func Short(topic string) string {
	sum := sha256.Sum256([]byte(topic))
	return hex.EncodeToString(sum[:3])
}

// cut shortens s to at most max bytes at a character boundary, marking the cut.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const mark = "…"
	s = s[:max-len(mark)]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + mark
}

type jsonMessage struct {
	Topic    string   `json:"topic"`
	Message  string   `json:"message"`
	Title    string   `json:"title,omitempty"`
	Priority int      `json:"priority,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

// ErrDisabled is returned when no server is set up.
var ErrDisabled = errors.New("ntfy is not set up on this bot")

// Publish sends a notification. Errors never contain the token.
func (c *Client) Publish(ctx context.Context, m Message) error {
	if c == nil || c.BaseURL == "" {
		return ErrDisabled
	}
	topic, err := c.Topic(m.Topic)
	if err != nil {
		return err
	}
	max := c.MaxBytes
	if max <= 0 {
		max = defaultBytes
	}
	body := strings.TrimSpace(m.Body)
	if body == "" {
		return errors.New("nothing to send")
	}
	jm := jsonMessage{Topic: topic, Message: cut(body, max), Title: cut(strings.TrimSpace(m.Title), maxTitle), Tags: m.Tags}
	if m.Priority >= 1 && m.Priority <= 5 && m.Priority != 3 {
		jm.Priority = m.Priority
	}
	payload, err := json.Marshal(jm)
	if err != nil {
		return err
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * backoff):
			}
		}
		retry, err := c.post(ctx, payload)
		if err == nil {
			return nil
		}
		last = err
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return last
}

// post makes one request and says whether another try could help.
func (c *Client) post(ctx context.Context, payload []byte) (retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(c.BaseURL, "/")+"/", bytes.NewReader(payload))
	if err != nil {
		return false, errors.New("the ntfy address is not valid")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "jannyq")
	switch {
	case c.Token == "":
	case strings.Contains(c.Token, ":"):
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(c.Token)))
	default:
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// A redirect would resend the request (and the credentials) somewhere the
	// operator did not choose: refuse, and say what to do.
	cl := *client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cl.Do(req)
	if err != nil {
		// What the user (and the model) is told must not reveal the address of
		// the server, which can be an internal one; the log has the details.
		if c.Log != nil {
			c.Log.Debug("ntfy request failed", "err", err)
		}
		return true, fmt.Errorf("could not reach the ntfy server (%s)", netReason(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode/100 == 2:
		return false, nil
	case resp.StatusCode/100 == 3:
		return false, fmt.Errorf("the ntfy server redirects (HTTP %d): give its final address, e.g. https://… instead of http://…", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return false, fmt.Errorf("the ntfy server refused (HTTP %d): the token is missing, wrong or not allowed to write to that topic", resp.StatusCode)
	default:
		msg := strings.TrimSpace(string(data))
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			msg = e.Error
		}
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500,
			fmt.Errorf("the ntfy server answered HTTP %d: %s", resp.StatusCode, msg)
	}
}

// netReason names a network failure in a few words, without addresses.
func netReason(err error) string {
	var dns *net.DNSError
	var tlsErr *tls.CertificateVerificationError
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timed out"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.As(err, &dns):
		return "the name could not be resolved"
	case errors.As(err, &tlsErr):
		return "the certificate is not trusted"
	default:
		return "network error"
	}
}
