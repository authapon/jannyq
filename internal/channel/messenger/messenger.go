// Package messenger implements a Facebook Messenger channel for a Page. Meta
// calls a webhook of the bot, which therefore needs a public HTTPS address;
// replies go out through the Graph API.
package messenger

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/channel/meta"
)

// maxTextRunes is below Messenger's 2000-character limit.
const maxTextRunes = 1900

const (
	profileTTL  = time.Hour
	maxProfiles = 5000
)

// Config configures the channel.
type Config struct {
	PageToken   string
	AppSecret   string
	VerifyToken string
	WebhookPath string // default /webhook/messenger
	GraphAPI    string // default meta.DefaultGraphAPI
	Client      *http.Client
}

// Channel is the Messenger adapter.
type Channel struct {
	cfg Config
	api *meta.Client
	rc  *meta.Receiver
	log *slog.Logger
	now func() time.Time

	profMu   sync.Mutex
	profiles map[string]profile
}

type profile struct {
	name string
	at   time.Time
}

// New creates the channel and registers its webhook on host.
func New(cfg Config, host meta.Host, log *slog.Logger) (*Channel, error) {
	if cfg.WebhookPath == "" {
		cfg.WebhookPath = "/webhook/messenger"
	}
	if cfg.GraphAPI == "" {
		cfg.GraphAPI = meta.DefaultGraphAPI
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Channel{cfg: cfg, api: &meta.Client{Base: cfg.GraphAPI, Token: cfg.PageToken, HTTP: cfg.Client},
		log: log.With("channel", "messenger"), now: time.Now, profiles: map[string]profile{}}
	rc, err := meta.NewReceiver(meta.ReceiverConfig{
		Name: "messenger", Path: cfg.WebhookPath, AppSecret: cfg.AppSecret, VerifyToken: cfg.VerifyToken, Parse: c.parse,
	}, host, c.log)
	if err != nil {
		return nil, err
	}
	c.rc = rc
	return c, nil
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "messenger" }

// Run implements channel.Channel.
func (c *Channel) Run(ctx context.Context, sink channel.Sink) error { return c.rc.Run(ctx, sink) }

type payload struct {
	Object string `json:"object"`
	Entry  []struct {
		ID        string `json:"id"`
		Messaging []struct {
			Sender    struct{ ID string } `json:"sender"`
			Recipient struct{ ID string } `json:"recipient"`
			Timestamp int64               `json:"timestamp"`
			Message   *struct {
				Mid         string `json:"mid"`
				Text        string `json:"text"`
				IsEcho      bool   `json:"is_echo"`
				Attachments []struct {
					Type    string `json:"type"`
					Payload struct {
						URL string `json:"url"`
					} `json:"payload"`
				} `json:"attachments"`
			} `json:"message"`
		} `json:"messaging"`
	} `json:"entry"`
}

// parse finds the messages people sent to the page: not echoes of the page's
// own replies, nor delivery and read receipts, nor postbacks.
func (c *Channel) parse(body []byte) ([]meta.Item, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	var items []meta.Item
	if p.Object != "page" {
		return nil, nil
	}
	for _, e := range p.Entry {
		for _, m := range e.Messaging {
			if m.Message == nil || m.Message.IsEcho || m.Sender.ID == "" || m.Sender.ID == e.ID {
				continue
			}
			sender, msg, ts := m.Sender.ID, m.Message, m.Timestamp
			items = append(items, meta.Item{Key: msg.Mid, Chat: sender, Build: func(ctx context.Context) channel.Incoming {
				in := channel.Incoming{
					Channel: "messenger", ChatID: sender, UserID: sender, Text: strings.TrimSpace(msg.Text), Addressed: true,
					ReceivedAt: time.Now(),
				}
				if ts > 0 {
					in.ReceivedAt = time.UnixMilli(ts)
				}
				for i, a := range msg.Attachments {
					switch a.Type {
					case "image", "file":
						name, mime := attachmentName(a.Payload.URL, a.Type, msg.Mid, i)
						in.Attachments = append(in.Attachments, channel.Attachment{Name: name, MIME: mime, Fetch: c.fetcher(a.Payload.URL)})
					case "fallback": // a shared link: nothing to read
					default: // audio, video, location, ...
						in.HasAttachment = true
					}
				}
				in.UserName = c.displayName(ctx, sender)
				in.Responder = &responder{c: c, psid: sender}
				return in
			}})
		}
	}
	return items, nil
}

// attachmentName derives a file name from the attachment's link, whose path
// ends with the original name for files.
func attachmentName(raw, kind, mid string, i int) (name, mime string) {
	if u, err := url.Parse(raw); err == nil {
		if base := path.Base(u.Path); base != "" && base != "." && base != "/" && strings.Contains(base, ".") && kind == "file" {
			return base, ""
		}
	}
	if kind == "image" {
		return "image-" + shortID(mid, i) + ".jpg", "image/jpeg"
	}
	return "file-" + shortID(mid, i), ""
}

func shortID(mid string, i int) string {
	var b strings.Builder
	for _, r := range mid {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if len(s) > 8 {
		s = s[len(s)-8:]
	}
	if s == "" {
		s = "x"
	}
	return s + "-" + string(rune('1'+i%9))
}

// fetcher downloads an attachment. Its link is public, so no token is sent.
func (c *Channel) fetcher(link string) func(context.Context, int64) ([]byte, error) {
	return func(ctx context.Context, max int64) ([]byte, error) {
		data, _, err := c.api.Download(ctx, link, max, false)
		return data, err
	}
}

// displayName looks up the sender's name, which needs a permission Meta grants
// only to some apps; without it people are simply called "user".
func (c *Channel) displayName(ctx context.Context, psid string) string {
	c.profMu.Lock()
	if p, ok := c.profiles[psid]; ok && c.now().Sub(p.at) < profileTTL {
		c.profMu.Unlock()
		return p.name
	}
	c.profMu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	var out struct {
		Name      string `json:"name"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	}
	name := "user"
	if err := c.api.Do(pctx, http.MethodGet, "/"+url.PathEscape(psid)+"?fields=name,first_name,last_name", nil, &out); err == nil {
		n := strings.TrimSpace(out.Name)
		if n == "" {
			n = strings.TrimSpace(out.FirstName + " " + out.LastName)
		}
		if n != "" {
			name = n
			c.profMu.Lock()
			if len(c.profiles) >= maxProfiles {
				c.profiles = map[string]profile{}
			}
			c.profiles[psid] = profile{name, c.now()}
			c.profMu.Unlock()
		}
	}
	return name
}

// ResponderFor implements channel.Notifier.
func (c *Channel) ResponderFor(chatID string, _ bool) (channel.Responder, error) {
	if chatID == "" {
		return nil, errors.New("messenger: no recipient")
	}
	return &responder{c: c, psid: chatID}, nil
}

// Window implements channel.Windowed: Messenger allows a message to a person
// within 24 hours of the person's last one.
func (*Channel) Window() time.Duration { return 24 * time.Hour }

type responder struct {
	c    *Channel
	psid string
}

// Typing shows the typing bubble (it lasts for about 20 seconds).
func (r *responder) Typing(ctx context.Context) error {
	return r.c.api.Do(ctx, http.MethodPost, "/me/messages", map[string]any{
		"recipient": map[string]string{"id": r.psid}, "sender_action": "typing_on",
	}, nil)
}

// Send delivers text as one or more messages.
func (r *responder) Send(ctx context.Context, text string) error {
	for _, part := range channel.Split(text, maxTextRunes) {
		err := r.c.api.Do(ctx, http.MethodPost, "/me/messages", map[string]any{
			"recipient":      map[string]string{"id": r.psid},
			"messaging_type": "RESPONSE",
			"message":        map[string]string{"text": part},
		}, nil)
		if err != nil {
			return err
		}
	}
	return nil
}
