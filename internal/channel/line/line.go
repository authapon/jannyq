// Package line implements a LINE Messaging API channel. LINE calls a webhook
// of the bot, which therefore needs a public HTTPS address; every request is
// checked against the channel secret before anything is read from it.
package line

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/webhook"
)

const (
	// maxTextRunes is below LINE's limit of 5000 UTF-16 units per text message.
	maxTextRunes = 4000
	// maxPerRequest is how many messages one reply or push call may carry.
	maxPerRequest = 5
	// replyTokenLife is how long a reply token is used; LINE accepts it for about a minute.
	replyTokenLife = 50 * time.Second
	profileTTL     = time.Hour
	maxProfiles    = 5000
	// badSignaturesPerMinute is how many refused requests one address may send before it is turned away.
	badSignaturesPerMinute = 30
)

// Host is the HTTP server the webhook is mounted on.
type Host interface {
	Mux() *http.ServeMux
	ClientKey(r *http.Request) string
	// ExemptFromRateLimit takes the webhook out of the shared per-address limit:
	// LINE's servers share a few addresses.
	ExemptFromRateLimit(paths ...string)
}

// Config configures the channel.
type Config struct {
	ChannelSecret string
	ChannelToken  string
	WebhookPath   string // default /webhook/line
	APIBase       string // default https://api.line.me
	DataAPIBase   string // default https://api-data.line.me
	Client        *http.Client
	// MaxInFlight bounds the messages being handled at once (default 64).
	MaxInFlight int
	// NoCommands stops "/reset" and friends from counting as addressed.
	NoCommands bool
}

// Channel is the LINE adapter.
type Channel struct {
	cfg     Config
	client  *http.Client
	log     *slog.Logger
	orderer *channel.Orderer
	dedupe  *webhook.Dedupe
	host    Host

	inFlight atomic.Int64
	bad      *webhook.Failures

	mu   sync.Mutex
	sink channel.Sink
	ctx  context.Context
	wg   sync.WaitGroup

	profMu   sync.Mutex
	profiles map[string]profile
	now      func() time.Time
}

type profile struct {
	name string
	at   time.Time
}

// New creates the channel and registers the webhook on host.
func New(cfg Config, host Host, log *slog.Logger) (*Channel, error) {
	if cfg.ChannelSecret == "" || cfg.ChannelToken == "" {
		return nil, errors.New("line: the channel secret and the channel access token are required")
	}
	if cfg.WebhookPath == "" {
		cfg.WebhookPath = "/webhook/line"
	}
	if !strings.HasPrefix(cfg.WebhookPath, "/") {
		cfg.WebhookPath = "/" + cfg.WebhookPath
	}
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.line.me"
	}
	if cfg.DataAPIBase == "" {
		cfg.DataAPIBase = "https://api-data.line.me"
	}
	cfg.APIBase, cfg.DataAPIBase = strings.TrimRight(cfg.APIBase, "/"), strings.TrimRight(cfg.DataAPIBase, "/")
	if cfg.MaxInFlight <= 0 {
		cfg.MaxInFlight = 64
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Channel{
		cfg: cfg, client: client, log: log.With("channel", "line"), host: host,
		orderer: channel.NewOrderer(), dedupe: webhook.NewDedupe(15*time.Minute, 20000),
		bad: webhook.NewFailures(badSignaturesPerMinute, time.Minute), profiles: map[string]profile{}, now: time.Now,
	}
	host.Mux().Handle("POST "+cfg.WebhookPath, http.HandlerFunc(c.handleWebhook))
	host.ExemptFromRateLimit(cfg.WebhookPath)
	return c, nil
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "line" }

// Run implements channel.Channel: it accepts events until ctx ends. The route
// was registered by New; serving belongs to the Host.
func (c *Channel) Run(ctx context.Context, sink channel.Sink) error {
	c.mu.Lock()
	c.sink, c.ctx = sink, ctx
	c.mu.Unlock()
	<-ctx.Done()
	c.wg.Wait()
	return nil
}

// --- webhook ---

type event struct {
	Type           string `json:"type"`
	Mode           string `json:"mode"`
	Timestamp      int64  `json:"timestamp"`
	WebhookEventID string `json:"webhookEventId"`
	ReplyToken     string `json:"replyToken"`
	Source         struct {
		Type    string `json:"type"`
		UserID  string `json:"userId"`
		GroupID string `json:"groupId"`
		RoomID  string `json:"roomId"`
	} `json:"source"`
	Message struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Text     string `json:"text"`
		FileName string `json:"fileName"`
		FileSize int64  `json:"fileSize"`
		Mention  *struct {
			Mentionees []struct {
				Index  int    `json:"index"`
				Length int    `json:"length"`
				Type   string `json:"type"`
				UserID string `json:"userId"`
				IsSelf bool   `json:"isSelf"`
			} `json:"mentionees"`
		} `json:"mention"`
		ContentProvider struct {
			Type string `json:"type"`
		} `json:"contentProvider"`
	} `json:"message"`
}

func (c *Channel) handleWebhook(w http.ResponseWriter, r *http.Request) {
	key := c.host.ClientKey(r)
	if c.bad.Blocked(key) {
		c.log.Warn("webhook refused: too many bad signatures from this client", "client", key)
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	webhook.Signed(webhook.DefaultMaxBody, func(r *http.Request, body []byte) bool {
		ok := webhook.VerifyBase64([]byte(c.cfg.ChannelSecret), body, r.Header.Get("X-Line-Signature"))
		if !ok {
			c.bad.Add(key)
			c.log.Warn("webhook refused: bad signature", "client", key)
		}
		return ok
	}, c.accept).ServeHTTP(w, r)
}

func (c *Channel) accept(w http.ResponseWriter, _ *http.Request, body []byte) {
	var payload struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	sink, ctx := c.sink, c.ctx
	c.mu.Unlock()
	if sink == nil {
		http.Error(w, "starting up", http.StatusServiceUnavailable) // LINE delivers again later
		return
	}
	received := c.now()
	for i := range payload.Events {
		ev := payload.Events[i]
		if c.dedupe.Seen(ev.WebhookEventID) {
			continue
		}
		if c.inFlight.Add(1) > int64(c.cfg.MaxInFlight) {
			c.inFlight.Add(-1)
			c.log.Warn("too many events at once: one was dropped")
			continue
		}
		in, name, ok := c.convert(ev, received)
		if !ok {
			c.inFlight.Add(-1)
			continue
		}
		wait, accepted := c.orderer.Enter(in.ChatID) // in arrival order, before the goroutine starts
		in.Accepted = accepted
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			defer c.inFlight.Add(-1)
			defer accepted()
			if wait != nil {
				select {
				case <-wait:
				case <-ctx.Done():
					return
				}
			}
			in.UserName = name(ctx)
			sink(ctx, in)
		}()
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, "{}")
}

// utf16Cut removes the range [index, index+length) of text, which LINE gives
// in UTF-16 code units.
func utf16Cut(text string, index, length int) string {
	u := utf16.Encode([]rune(text))
	if index < 0 || length <= 0 || index > len(u) {
		return text
	}
	end := min(index+length, len(u))
	return string(utf16.Decode(append(append([]uint16{}, u[:index]...), u[end:]...)))
}

var commands = map[string]bool{"start": true, "help": true, "reset": true, "compact": true}

func isCommand(text string) bool {
	f := strings.Fields(text)
	return len(f) > 0 && strings.HasPrefix(f[0], "/") && commands[strings.ToLower(f[0][1:])]
}

// convert turns an event into an Incoming; false means that it is not for us.
func (c *Channel) convert(ev event, received time.Time) (in channel.Incoming, name func(context.Context) string, ok bool) {
	if ev.Type != "message" || ev.Mode == "standby" {
		return in, nil, false
	}
	var chatID string
	isGroup := false
	switch ev.Source.Type {
	case "user":
		chatID = ev.Source.UserID
	case "group":
		chatID, isGroup = ev.Source.GroupID, true
	case "room":
		chatID, isGroup = ev.Source.RoomID, true
	}
	if chatID == "" {
		return in, nil, false
	}
	userID := ev.Source.UserID
	if userID == "" { // people in a group who have not agreed to share their identity
		userID = "unknown:" + chatID
	}
	in = channel.Incoming{
		Channel: "line", ChatID: chatID, UserID: userID, IsGroup: isGroup, Addressed: !isGroup,
		ReceivedAt: received,
	}
	if ev.Timestamp > 0 {
		in.ReceivedAt = time.UnixMilli(ev.Timestamp)
	}
	m := ev.Message
	switch m.Type {
	case "text":
		text := m.Text
		if m.Mention != nil {
			ms := m.Mention.Mentionees
			sort.Slice(ms, func(i, j int) bool { return ms[i].Index > ms[j].Index }) // cut from the end so that indexes stay valid
			for _, me := range ms {
				if me.IsSelf {
					in.Addressed = true
					text = utf16Cut(text, me.Index, me.Length)
				}
			}
		}
		in.Text = strings.TrimSpace(text)
		if !c.cfg.NoCommands && isCommand(in.Text) {
			in.Addressed = true
		}
	case "image":
		in.Attachments = []channel.Attachment{{Name: "image-" + m.ID + ".jpg", MIME: "image/jpeg", Fetch: c.fetcher(m.ID, 0)}}
	case "file":
		in.Attachments = []channel.Attachment{{Name: m.FileName, Size: m.FileSize, Fetch: c.fetcher(m.ID, m.FileSize)}}
	default: // video, audio, location, sticker, ...
		in.HasAttachment = true
	}
	in.Responder = &responder{c: c, chatID: chatID, isGroup: isGroup, token: ev.ReplyToken, tokenAt: received}
	sourceType, senderID := ev.Source.Type, ev.Source.UserID
	// The name needs a call to LINE: it is looked up after the webhook has been answered.
	return in, func(ctx context.Context) string { return c.displayName(ctx, sourceType, chatID, senderID) }, true
}

// --- LINE API ---

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("line: HTTP %d %s", e.Status, e.Message) }

func (c *Channel) request(ctx context.Context, method, base, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.ChannelToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return c.client.Do(req)
}

func (c *Channel) post(ctx context.Context, path string, body any) error {
	resp, err := c.request(ctx, http.MethodPost, c.cfg.APIBase, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 == 2 {
		return nil
	}
	var e struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &e)
	return &apiError{Status: resp.StatusCode, Message: e.Message}
}

// displayName looks up a sender's name, remembering it for an hour.
func (c *Channel) displayName(ctx context.Context, sourceType, chatID, userID string) string {
	if userID == "" {
		return "user"
	}
	key := chatID + "|" + userID
	c.profMu.Lock()
	if p, ok := c.profiles[key]; ok && c.now().Sub(p.at) < profileTTL {
		c.profMu.Unlock()
		return p.name
	}
	c.profMu.Unlock()

	path := "/v2/bot/profile/" + url.PathEscape(userID)
	switch sourceType {
	case "group":
		path = "/v2/bot/group/" + url.PathEscape(chatID) + "/member/" + url.PathEscape(userID)
	case "room":
		path = "/v2/bot/room/" + url.PathEscape(chatID) + "/member/" + url.PathEscape(userID)
	}
	pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	name := "user"
	if resp, err := c.request(pctx, http.MethodGet, c.cfg.APIBase, path, nil); err == nil {
		var p struct {
			DisplayName string `json:"displayName"`
		}
		if resp.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&p) == nil && strings.TrimSpace(p.DisplayName) != "" {
			name = strings.TrimSpace(p.DisplayName)
			c.profMu.Lock()
			if len(c.profiles) >= maxProfiles {
				c.profiles = map[string]profile{}
			}
			c.profiles[key] = profile{name, c.now()}
			c.profMu.Unlock()
		}
		resp.Body.Close()
	}
	return name
}

// fetcher downloads the content of an image or file message.
func (c *Channel) fetcher(messageID string, declared int64) func(context.Context, int64) ([]byte, error) {
	return func(ctx context.Context, max int64) ([]byte, error) {
		if declared > max {
			return nil, channel.ErrTooLarge
		}
		resp, err := c.request(ctx, http.MethodGet, c.cfg.DataAPIBase, "/v2/bot/message/"+url.PathEscape(messageID)+"/content", nil)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("line: downloading a file failed (HTTP %d)", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err != nil {
			return nil, err
		}
		if int64(len(data)) > max {
			return nil, channel.ErrTooLarge
		}
		return data, nil
	}
}

// --- replies ---

type responder struct {
	c       *Channel
	chatID  string
	isGroup bool

	mu         sync.Mutex
	token      string // reply token, until used
	tokenAt    time.Time
	lastTyping time.Time
}

// Typing shows LINE's loading animation, which exists in one-to-one chats only.
func (r *responder) Typing(ctx context.Context) error {
	if r.isGroup {
		return nil
	}
	r.mu.Lock()
	if r.c.now().Sub(r.lastTyping) < 15*time.Second {
		r.mu.Unlock()
		return nil
	}
	r.lastTyping = r.c.now()
	r.mu.Unlock()
	return r.c.post(ctx, "/v2/bot/chat/loading/start", map[string]any{"chatId": r.chatID, "loadingSeconds": 20})
}

type textMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Send delivers text. The first call within the life of the reply token uses
// it (replies are free of charge); anything else is pushed, which counts against
// the monthly message quota of the LINE account.
func (r *responder) Send(ctx context.Context, text string) error {
	var msgs []textMessage
	for _, part := range channel.Split(text, maxTextRunes) {
		msgs = append(msgs, textMessage{"text", part})
	}
	for len(msgs) > 0 {
		n := min(len(msgs), maxPerRequest)
		batch := msgs[:n]
		msgs = msgs[n:]

		r.mu.Lock()
		token := r.token
		if token != "" && r.c.now().Sub(r.tokenAt) > replyTokenLife {
			token = ""
		}
		r.token = "" // a reply token works once
		r.mu.Unlock()

		if token != "" {
			err := r.c.post(ctx, "/v2/bot/message/reply", map[string]any{"replyToken": token, "messages": batch})
			if err == nil {
				continue
			}
			var ae *apiError
			if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest { // not an expired token: do not retry blindly
				return err
			}
			r.c.log.Info("the reply token was refused; pushing instead", "reason", ae.Message)
		}
		if err := r.c.post(ctx, "/v2/bot/message/push", map[string]any{"to": r.chatID, "messages": batch}); err != nil {
			return err
		}
	}
	return nil
}
