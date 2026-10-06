// Package whatsapp implements a WhatsApp channel on the WhatsApp Business
// Cloud API. Meta calls a webhook of the bot, which therefore needs a public
// HTTPS address; replies go out through the Graph API.
package whatsapp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/channel/meta"
)

// maxTextRunes is below WhatsApp's 4096-character limit.
const maxTextRunes = 4000

// Config configures the channel.
type Config struct {
	AccessToken   string
	PhoneNumberID string // the business number's id (not the phone number)
	AppSecret     string
	VerifyToken   string
	WebhookPath   string // default /webhook/whatsapp
	GraphAPI      string // default meta.DefaultGraphAPI
	Client        *http.Client
}

// Channel is the WhatsApp adapter.
type Channel struct {
	cfg Config
	api *meta.Client
	rc  *meta.Receiver
	log *slog.Logger
}

// New creates the channel and registers its webhook on host.
func New(cfg Config, host meta.Host, log *slog.Logger) (*Channel, error) {
	if cfg.WebhookPath == "" {
		cfg.WebhookPath = "/webhook/whatsapp"
	}
	if cfg.GraphAPI == "" {
		cfg.GraphAPI = meta.DefaultGraphAPI
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Channel{cfg: cfg, api: &meta.Client{Base: cfg.GraphAPI, Token: cfg.AccessToken, HTTP: cfg.Client}, log: log.With("channel", "whatsapp")}
	rc, err := meta.NewReceiver(meta.ReceiverConfig{
		Name: "whatsapp", Path: cfg.WebhookPath, AppSecret: cfg.AppSecret, VerifyToken: cfg.VerifyToken, Parse: c.parse,
	}, host, c.log)
	if err != nil {
		return nil, err
	}
	c.rc = rc
	return c, nil
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "whatsapp" }

// Run implements channel.Channel.
func (c *Channel) Run(ctx context.Context, sink channel.Sink) error { return c.rc.Run(ctx, sink) }

type media struct {
	ID       string `json:"id"`
	Filename string `json:"filename"`
	MIMEType string `json:"mime_type"`
	Caption  string `json:"caption"`
}

type payload struct {
	Object string `json:"object"`
	Entry  []struct {
		Changes []struct {
			Field string `json:"field"`
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Contacts []struct {
					WaID    string `json:"wa_id"`
					Profile struct {
						Name string `json:"name"`
					} `json:"profile"`
				} `json:"contacts"`
				Messages []struct {
					From      string `json:"from"`
					ID        string `json:"id"`
					Timestamp string `json:"timestamp"`
					Type      string `json:"type"`
					Text      *struct {
						Body string `json:"body"`
					} `json:"text"`
					Image    *media `json:"image"`
					Document *media `json:"document"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

// parse finds the messages people sent to the business number. Status
// updates (sent, delivered, read) and messages for other numbers are ignored.
func (c *Channel) parse(body []byte) ([]meta.Item, error) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, err
	}
	if p.Object != "whatsapp_business_account" {
		return nil, nil
	}
	var items []meta.Item
	for _, e := range p.Entry {
		for _, ch := range e.Changes {
			v := ch.Value
			if ch.Field != "messages" || v.Metadata.PhoneNumberID != c.cfg.PhoneNumberID {
				continue
			}
			names := map[string]string{}
			for _, ct := range v.Contacts {
				names[ct.WaID] = strings.TrimSpace(ct.Profile.Name)
			}
			for _, m := range v.Messages {
				if m.From == "" || m.ID == "" {
					continue
				}
				from, id, typ := m.From, m.ID, m.Type
				var text string
				var file *media
				switch typ {
				case "text":
					if m.Text != nil {
						text = strings.TrimSpace(m.Text.Body)
					}
				case "image":
					file = m.Image
				case "document":
					file = m.Document
				case "reaction", "unsupported", "system", "ephemeral":
					continue // not something said to the bot
				}
				if file != nil {
					text = strings.TrimSpace(file.Caption)
				}
				name, ts := names[from], m.Timestamp
				fileCopy := file
				items = append(items, meta.Item{Key: id, Chat: from, Build: func(context.Context) channel.Incoming {
					in := channel.Incoming{
						Channel: "whatsapp", ChatID: from, UserID: from, UserName: name, Text: text, Addressed: true,
						ReceivedAt: time.Now(),
					}
					if sec, err := strconv.ParseInt(ts, 10, 64); err == nil && sec > 0 {
						in.ReceivedAt = time.Unix(sec, 0)
					}
					if in.UserName == "" {
						in.UserName = "user"
					}
					switch {
					case fileCopy != nil && fileCopy.ID != "":
						fname := fileCopy.Filename
						if fname == "" && typ == "image" {
							fname = "image-" + sanitizeID(fileCopy.ID) + ".jpg"
						}
						in.Attachments = []channel.Attachment{{Name: fname, MIME: fileCopy.MIMEType, Fetch: c.fetcher(fileCopy.ID)}}
					case typ != "text":
						in.HasAttachment = true // audio, video, sticker, location, contacts, ...
					}
					in.Responder = &responder{c: c, to: from, messageID: id}
					return in
				}})
			}
		}
	}
	return items, nil
}

func sanitizeID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() > 10 {
		return b.String()[:10]
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// fetcher downloads a media file: the Graph API gives its address, and the
// address needs the access token too.
func (c *Channel) fetcher(mediaID string) func(context.Context, int64) ([]byte, error) {
	return func(ctx context.Context, max int64) ([]byte, error) {
		var info struct {
			URL      string `json:"url"`
			FileSize int64  `json:"file_size"`
		}
		if err := c.api.Do(ctx, http.MethodGet, "/"+url.PathEscape(mediaID), nil, &info); err != nil {
			return nil, err
		}
		if info.FileSize > max {
			return nil, channel.ErrTooLarge
		}
		if info.URL == "" {
			return nil, &meta.Error{Message: "no download address for the media"}
		}
		data, _, err := c.api.Download(ctx, info.URL, max, true)
		return data, err
	}
}

type responder struct {
	c         *Channel
	to        string
	messageID string
}

func (r *responder) path() string { return "/" + url.PathEscape(r.c.cfg.PhoneNumberID) + "/messages" }

// Typing marks the message as read and shows the typing indicator, which lasts
// for about 25 seconds.
func (r *responder) Typing(ctx context.Context) error {
	return r.c.api.Do(ctx, http.MethodPost, r.path(), map[string]any{
		"messaging_product": "whatsapp", "status": "read", "message_id": r.messageID,
		"typing_indicator": map[string]string{"type": "text"},
	}, nil)
}

// Send delivers text as one or more messages.
func (r *responder) Send(ctx context.Context, text string) error {
	for _, part := range channel.Split(text, maxTextRunes) {
		err := r.c.api.Do(ctx, http.MethodPost, r.path(), map[string]any{
			"messaging_product": "whatsapp", "recipient_type": "individual", "to": r.to,
			"type": "text", "text": map[string]any{"body": part, "preview_url": false},
		}, nil)
		if err != nil {
			return err
		}
	}
	return nil
}
