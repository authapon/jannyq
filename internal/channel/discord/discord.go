// Package discord implements a Discord channel: it receives messages over the
// gateway (a WebSocket the bot opens itself, so no public URL is needed) and
// answers through the REST API.
package discord

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/authapon/jannyq/internal/channel"
)

// maxMessageRunes is below Discord's 2000-character limit, with a margin.
const maxMessageRunes = 1900

// Gateway intents: messages of servers and of direct chats, and their text.
// MESSAGE_CONTENT is a privileged intent that must be switched on for the bot in
// the developer portal; the GUILDS intent is left out on purpose, as it makes
// Discord send a very large description of every server at start-up.
const (
	intentGuildMessages  = 1 << 9
	intentDirectMessages = 1 << 12
	intentMessageContent = 1 << 15
)

// Config configures the channel.
type Config struct {
	Token      string
	APIBase    string // default https://discord.com/api/v10
	GatewayURL string // default wss://gateway.discord.gg
	Client     *http.Client
	// Version appears in the User-Agent.
	Version string
	// NoCommands leaves "!reset" and "/reset" as ordinary text.
	NoCommands bool
}

// Channel is the Discord adapter.
type Channel struct {
	cfg    Config
	client *http.Client
	log    *slog.Logger

	mu      sync.Mutex
	botID   string
	botName string
}

// New creates the channel.
func New(cfg Config, log *slog.Logger) *Channel {
	if cfg.APIBase == "" {
		cfg.APIBase = "https://discord.com/api/v10"
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	if cfg.GatewayURL == "" {
		cfg.GatewayURL = "wss://gateway.discord.gg"
	}
	cfg.GatewayURL = strings.TrimRight(cfg.GatewayURL, "/")
	if cfg.Version == "" {
		cfg.Version = "dev"
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	if log == nil {
		log = slog.Default()
	}
	return &Channel{cfg: cfg, client: client, log: log.With("channel", "discord")}
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "discord" }

func (c *Channel) bot() (id, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.botID, c.botName
}

// --- gateway ---

type gatewayPayload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
	S  *int64          `json:"s,omitempty"`
	T  string          `json:"t,omitempty"`
}

// resumeState is what is needed to continue a gateway session after a
// dropped connection without missing messages.
type resumeState struct {
	sessionID string
	url       string
	seq       int64
}

// Fatal gateway close codes: retrying cannot help.
var fatalClose = map[websocket.StatusCode]string{
	4004: "Discord rejected the bot token",
	4010: "invalid shard",
	4011: "the bot is in too many servers for one connection (sharding is not supported)",
	4012: "invalid gateway version",
	4013: "invalid intents",
	4014: "the MESSAGE CONTENT intent is not enabled: switch it on for the bot in the Discord developer portal (Bot → Privileged Gateway Intents)",
}

var errReconnect = errors.New("discord: the gateway asked to reconnect")

// Run implements channel.Channel.
func (c *Channel) Run(ctx context.Context, sink channel.Sink) error {
	var wg sync.WaitGroup
	defer wg.Wait()
	orderer := channel.NewOrderer()
	dispatch := func(m *message) {
		in, ok := c.convert(m)
		if !ok {
			return
		}
		wait, accepted := orderer.Enter(in.ChatID)
		in.Accepted = accepted
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer accepted()
			if wait != nil {
				select {
				case <-wait:
				case <-ctx.Done():
					return
				}
			}
			sink(ctx, in)
		}()
	}

	var state resumeState
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := c.session(ctx, &state, dispatch)
		if ctx.Err() != nil {
			return nil
		}
		if code := websocket.CloseStatus(err); code != -1 {
			if msg, fatal := fatalClose[code]; fatal {
				return fmt.Errorf("discord: %s (gateway close code %d)", msg, code)
			}
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second // it was a healthy connection that dropped
		}
		c.log.Warn("gateway connection ended", "err", err, "retry_in", backoff)
		sleep(ctx, backoff)
		if backoff < time.Minute {
			backoff *= 2
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// session runs one gateway connection until it ends.
func (c *Channel) session(ctx context.Context, st *resumeState, dispatch func(*message)) error {
	base := c.cfg.GatewayURL
	if st.url != "" {
		base = strings.TrimRight(st.url, "/")
	}
	conn, _, err := websocket.Dial(ctx, base+"/?v=10&encoding=json", &websocket.DialOptions{HTTPClient: c.client})
	if err != nil {
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	read := func() (gatewayPayload, error) {
		var p gatewayPayload
		_, data, err := conn.Read(ctx)
		if err != nil {
			return p, err
		}
		return p, json.Unmarshal(data, &p)
	}
	write := func(v any) error {
		b, _ := json.Marshal(v)
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		defer wcancel()
		return conn.Write(wctx, websocket.MessageText, b)
	}

	hello, err := read()
	if err != nil {
		return err
	}
	if hello.Op != 10 {
		return fmt.Errorf("discord: expected hello, got op %d", hello.Op)
	}
	var h struct {
		Interval int `json:"heartbeat_interval"`
	}
	if err := json.Unmarshal(hello.D, &h); err != nil || h.Interval <= 0 {
		return errors.New("discord: bad hello")
	}

	var seq int64 = st.seq
	var seqMu sync.Mutex
	curSeq := func() any {
		seqMu.Lock()
		defer seqMu.Unlock()
		if seq == 0 {
			return nil
		}
		return seq
	}
	acked := make(chan struct{}, 1)
	acked <- struct{}{}
	beat := func() error {
		return write(map[string]any{"op": 1, "d": curSeq()})
	}
	go func() { // heartbeats; a missing acknowledgement means the connection is dead
		interval := time.Duration(h.Interval) * time.Millisecond
		first := time.NewTimer(interval / 2)
		defer first.Stop()
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-acked:
			default:
				c.log.Warn("no heartbeat acknowledgement: reconnecting")
				conn.Close(websocket.StatusCode(4000), "no heartbeat ack")
				return
			}
			if err := beat(); err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()

	if st.sessionID != "" {
		err = write(map[string]any{"op": 6, "d": map[string]any{"token": c.cfg.Token, "session_id": st.sessionID, "seq": st.seq}})
	} else {
		err = write(map[string]any{"op": 2, "d": map[string]any{
			"token":      c.cfg.Token,
			"intents":    intentGuildMessages | intentDirectMessages | intentMessageContent,
			"properties": map[string]string{"os": "linux", "browser": "jannyq", "device": "jannyq"},
		}})
	}
	if err != nil {
		return err
	}

	for {
		p, err := read()
		if err != nil {
			return err
		}
		switch p.Op {
		case 0:
			if p.S != nil {
				seqMu.Lock()
				seq = *p.S
				st.seq = seq
				seqMu.Unlock()
			}
			c.handleDispatch(p, st, dispatch)
		case 1: // the server wants a heartbeat now
			if err := beat(); err != nil {
				return err
			}
		case 11:
			select {
			case acked <- struct{}{}:
			default:
			}
		case 7:
			return errReconnect
		case 9:
			var resumable bool
			_ = json.Unmarshal(p.D, &resumable)
			if !resumable {
				*st = resumeState{}
			}
			sleep(ctx, 2*time.Second) // Discord asks for a pause of one to five seconds
			return errors.New("discord: invalid session")
		}
	}
}

func (c *Channel) handleDispatch(p gatewayPayload, st *resumeState, dispatch func(*message)) {
	switch p.T {
	case "READY":
		var r struct {
			SessionID   string `json:"session_id"`
			ResumeURL   string `json:"resume_gateway_url"`
			User        user   `json:"user"`
			Application struct {
				ID string `json:"id"`
			} `json:"application"`
		}
		if json.Unmarshal(p.D, &r) != nil {
			return
		}
		st.sessionID, st.url = r.SessionID, r.ResumeURL
		c.mu.Lock()
		c.botID, c.botName = r.User.ID, r.User.Username
		c.mu.Unlock()
		c.log.Info("discord bot connected", "username", r.User.Username)
	case "MESSAGE_CREATE":
		var m message
		if err := json.Unmarshal(p.D, &m); err != nil {
			c.log.Warn("unreadable message event", "err", err)
			return
		}
		dispatch(&m)
	}
}

// --- messages ---

type user struct {
	ID         string `json:"id"`
	Username   string `json:"username"`
	GlobalName string `json:"global_name"`
	Bot        bool   `json:"bot"`
}

type attachment struct {
	ID          string `json:"id"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
}

type message struct {
	ID        string `json:"id"`
	ChannelID string `json:"channel_id"`
	GuildID   string `json:"guild_id"`
	Author    user   `json:"author"`
	Member    *struct {
		Nick string `json:"nick"`
	} `json:"member"`
	Content           string       `json:"content"`
	Timestamp         string       `json:"timestamp"`
	Type              int          `json:"type"`
	WebhookID         string       `json:"webhook_id"`
	Mentions          []user       `json:"mentions"`
	Attachments       []attachment `json:"attachments"`
	ReferencedMessage *message     `json:"referenced_message"`
}

func displayName(u user, nick string) string {
	for _, n := range []string{nick, u.GlobalName, u.Username} {
		if n = strings.TrimSpace(n); n != "" {
			return n
		}
	}
	return "user"
}

// commands are the chat commands, which can be written "!reset" because Discord
// keeps text starting with "/" for its own slash commands.
var commands = map[string]bool{"start": true, "help": true, "reset": true, "compact": true}

// convert turns a gateway message into an Incoming; false means to ignore it.
func (c *Channel) convert(m *message) (channel.Incoming, bool) {
	botID, _ := c.bot()
	if m.Author.ID == "" || m.Author.Bot || m.WebhookID != "" || (botID != "" && m.Author.ID == botID) {
		return channel.Incoming{}, false
	}
	if m.Type != 0 && m.Type != 19 { // plain messages and replies only (not joins, pins, ...)
		return channel.Incoming{}, false
	}
	isGroup := m.GuildID != ""
	addressed := !isGroup
	if m.ReferencedMessage != nil && botID != "" && m.ReferencedMessage.Author.ID == botID {
		addressed = true
	}

	text := m.Content
	for _, u := range m.Mentions {
		raw1, raw2 := "<@"+u.ID+">", "<@!"+u.ID+">"
		if botID != "" && u.ID == botID {
			if strings.Contains(text, raw1) || strings.Contains(text, raw2) {
				addressed = true
			}
			text = strings.NewReplacer(raw1, "", raw2, "").Replace(text)
			continue
		}
		name := "@" + displayName(u, "")
		text = strings.NewReplacer(raw1, name, raw2, name).Replace(text)
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "!") && !c.cfg.NoCommands {
		fields := strings.Fields(text[1:])
		if len(fields) > 0 && commands[strings.ToLower(fields[0])] {
			text = "/" + strings.ToLower(fields[0]) + " " + strings.Join(fields[1:], " ")
			text = strings.TrimSpace(text)
			addressed = true
		}
	}
	if strings.HasPrefix(text, "/") && isGroup && !c.cfg.NoCommands {
		addressed = addressed || isCommandText(text)
	}

	nick := ""
	if m.Member != nil {
		nick = m.Member.Nick
	}
	var sentAt time.Time
	if t, err := time.Parse(time.RFC3339, m.Timestamp); err == nil {
		sentAt = t
	}
	in := channel.Incoming{
		Channel:    "discord",
		ChatID:     m.ChannelID,
		UserID:     m.Author.ID,
		UserName:   displayName(m.Author, nick),
		Text:       text,
		IsGroup:    isGroup,
		Addressed:  addressed,
		ReceivedAt: sentAt,
		Responder:  &responder{c: c, channelID: m.ChannelID, replyTo: m.ID, isGroup: isGroup},
	}
	for _, a := range m.Attachments {
		in.Attachments = append(in.Attachments, channel.Attachment{
			Name: a.Filename, MIME: a.ContentType, Size: a.Size, Fetch: c.fetcher(a.URL, a.Size),
		})
	}
	if text == "" && len(in.Attachments) == 0 && isGroup && !addressed {
		return channel.Incoming{}, false // nothing to keep (an embed or sticker only)
	}
	return in, true
}

func isCommandText(text string) bool {
	f := strings.Fields(text)
	return len(f) > 0 && commands[strings.ToLower(strings.TrimPrefix(f[0], "/"))]
}

// fetcher downloads an attachment from Discord's CDN, whose links are public.
func (c *Channel) fetcher(url string, declared int64) func(context.Context, int64) ([]byte, error) {
	return func(ctx context.Context, max int64) ([]byte, error) {
		if declared > max {
			return nil, channel.ErrTooLarge
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("discord: downloading an attachment failed (HTTP %d)", resp.StatusCode)
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

// --- REST ---

type apiError struct {
	Status     int
	Message    string
	RetryAfter float64
}

func (e *apiError) Error() string { return fmt.Sprintf("discord: HTTP %d %s", e.Status, e.Message) }

// call performs a REST request, waiting out rate limits (429) a few times.
func (c *Channel) call(ctx context.Context, method, path string, body any) error {
	var payload []byte
	if body != nil {
		payload, _ = json.Marshal(body)
	}
	for attempt := 0; ; attempt++ {
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.cfg.APIBase+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bot "+c.cfg.Token)
		req.Header.Set("User-Agent", "DiscordBot (https://github.com/authapon/jannyq, "+c.cfg.Version+")")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode/100 == 2 {
			return nil
		}
		ae := &apiError{Status: resp.StatusCode}
		var e struct {
			Message    string  `json:"message"`
			RetryAfter float64 `json:"retry_after"`
		}
		if json.Unmarshal(data, &e) == nil {
			ae.Message, ae.RetryAfter = e.Message, e.RetryAfter
		}
		if ae.RetryAfter == 0 {
			if v, err := strconv.ParseFloat(resp.Header.Get("Retry-After"), 64); err == nil {
				ae.RetryAfter = v
			}
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 3 && ae.RetryAfter <= 30 {
			wait := time.Duration((ae.RetryAfter + 0.1) * float64(time.Second))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return ae
	}
}

// ResponderFor implements channel.Notifier.
func (c *Channel) ResponderFor(chatID string, _ bool) (channel.Responder, error) {
	if chatID == "" {
		return nil, errors.New("discord: no channel id")
	}
	return &responder{c: c, channelID: chatID}, nil
}

type responder struct {
	c         *Channel
	channelID string
	replyTo   string
	isGroup   bool
}

func (r *responder) Typing(ctx context.Context) error {
	return r.c.call(ctx, http.MethodPost, "/channels/"+r.channelID+"/typing", nil)
}

func (r *responder) Send(ctx context.Context, text string) error {
	for i, part := range channel.Split(text, maxMessageRunes) {
		body := map[string]any{
			"content": part,
			// a reply from the model must never be able to ping @everyone, roles or people
			"allowed_mentions": map[string]any{"parse": []string{}},
		}
		if i == 0 && r.isGroup {
			body["message_reference"] = map[string]any{"message_id": r.replyTo, "fail_if_not_exists": false}
		}
		if err := r.c.call(ctx, http.MethodPost, "/channels/"+r.channelID+"/messages", body); err != nil {
			return err
		}
	}
	return nil
}
