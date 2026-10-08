// Package telegram implements a Telegram Bot API channel using long polling,
// so no public URL is needed.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/authapon/jannyq/internal/channel"
)

// maxMessageRunes is below Telegram's 4096 limit to leave a safety margin.
const maxMessageRunes = 4000

// Config configures the channel.
type Config struct {
	Token       string
	APIBase     string        // default https://api.telegram.org
	PollTimeout time.Duration // long-poll duration, default 30s
	Client      *http.Client  // optional
	// NoCommands stops "/…" text from counting as addressed to the bot.
	NoCommands bool
}

// Channel is the Telegram adapter.
type Channel struct {
	cfg     Config
	client  *http.Client
	log     *slog.Logger
	botID   int64
	botUser string

	albumMu sync.Mutex
	albums  map[albumKey]*album
}

// New creates the channel.
func New(cfg Config, log *slog.Logger) *Channel {
	if cfg.APIBase == "" {
		cfg.APIBase = "https://api.telegram.org"
	}
	cfg.APIBase = strings.TrimRight(cfg.APIBase, "/")
	if cfg.PollTimeout <= 0 {
		cfg.PollTimeout = 30 * time.Second
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{} // per-request deadlines come from contexts
	}
	if log == nil {
		log = slog.Default()
	}
	return &Channel{cfg: cfg, client: client, log: log.With("channel", "telegram")}
}

// Name implements channel.Channel.
func (c *Channel) Name() string { return "telegram" }

type apiResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
	ErrorCode   int             `json:"error_code"`
	Parameters  struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters"`
}

// apiError is a Bot API error response.
type apiError struct {
	Code        int
	Description string
	RetryAfter  int
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Description) }

// call invokes a Bot API method. Errors are scrubbed of the bot token, which
// is part of the request URL.
func (c *Channel) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	url := c.cfg.APIBase + "/bot" + c.cfg.Token + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return c.redact(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return c.redact(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return c.redact(err)
	}
	var ar apiResponse
	if err := json.Unmarshal(data, &ar); err != nil {
		return fmt.Errorf("telegram: unexpected response (HTTP %d)", resp.StatusCode)
	}
	if !ar.OK {
		return &apiError{Code: ar.ErrorCode, Description: ar.Description, RetryAfter: ar.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(ar.Result, out)
	}
	return nil
}

func (c *Channel) redact(err error) error {
	if err == nil || c.cfg.Token == "" {
		return err
	}
	msg := strings.ReplaceAll(err.Error(), c.cfg.Token, "***")
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", msg, ctxErr(err))
	}
	return errors.New(msg)
}

func ctxErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return context.Canceled
}

type tgUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type tgChat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type tgMessage struct {
	MessageID       int64      `json:"message_id"`
	Date            int64      `json:"date"`
	MessageThreadID int64      `json:"message_thread_id"`
	IsTopicMessage  bool       `json:"is_topic_message"`
	From            *tgUser    `json:"from"`
	Chat            tgChat     `json:"chat"`
	Text            string     `json:"text"`
	Caption         string     `json:"caption"`
	MediaGroupID    string     `json:"media_group_id"`
	ReplyToMessage  *tgMessage `json:"reply_to_message"`

	// Presence of any of these means the message carries media.
	Photo     json.RawMessage `json:"photo"`
	Document  *tgDocument     `json:"document"`
	Voice     json.RawMessage `json:"voice"`
	Audio     json.RawMessage `json:"audio"`
	Video     json.RawMessage `json:"video"`
	VideoNote json.RawMessage `json:"video_note"`
	Animation json.RawMessage `json:"animation"`
	Sticker   json.RawMessage `json:"sticker"`
}

type tgDocument struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

type tgPhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize int64  `json:"file_size"`
}

func (m *tgMessage) hasMedia() bool {
	if m.Document != nil {
		return true
	}
	for _, r := range []json.RawMessage{m.Photo, m.Voice, m.Audio, m.Video, m.VideoNote, m.Animation, m.Sticker} {
		if len(r) > 0 && string(r) != "null" {
			return true
		}
	}
	return false
}

// attachments lists the files of a message that can be fetched: the largest
// size of a photo, or a document.
func (c *Channel) attachments(m *tgMessage) []channel.Attachment {
	var out []channel.Attachment
	if len(m.Photo) > 0 {
		var sizes []tgPhotoSize
		if json.Unmarshal(m.Photo, &sizes) == nil {
			best := -1
			for i, s := range sizes {
				if s.FileID != "" && (best < 0 || s.Width*s.Height > sizes[best].Width*sizes[best].Height) {
					best = i
				}
			}
			if best >= 0 {
				s := sizes[best]
				out = append(out, channel.Attachment{
					Name: fmt.Sprintf("photo-%d.jpg", m.MessageID), MIME: "image/jpeg", Size: s.FileSize,
					Fetch: c.fetcher(s.FileID, s.FileSize),
				})
			}
		}
	}
	if d := m.Document; d != nil && d.FileID != "" {
		out = append(out, channel.Attachment{Name: d.FileName, MIME: d.MIMEType, Size: d.FileSize, Fetch: c.fetcher(d.FileID, d.FileSize)})
	}
	return out
}

// fetcher returns a function that downloads a file through getFile. Bot API
// downloads are limited to 20 MB by Telegram itself.
func (c *Channel) fetcher(fileID string, declared int64) func(context.Context, int64) ([]byte, error) {
	return func(ctx context.Context, max int64) ([]byte, error) {
		if declared > max {
			return nil, channel.ErrTooLarge
		}
		var f struct {
			FilePath string `json:"file_path"`
			FileSize int64  `json:"file_size"`
		}
		if err := c.call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
			var ae *apiError
			if errors.As(err, &ae) && strings.Contains(strings.ToLower(ae.Description), "too big") {
				return nil, channel.ErrTooLarge
			}
			return nil, err
		}
		if f.FilePath == "" {
			return nil, errors.New("telegram: getFile returned no path")
		}
		if f.FileSize > max {
			return nil, channel.ErrTooLarge
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.cfg.APIBase+"/file/bot"+c.cfg.Token+"/"+f.FilePath, nil)
		if err != nil {
			return nil, c.redact(err)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return nil, c.redact(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("telegram: downloading a file failed (HTTP %d)", resp.StatusCode)
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
		if err != nil {
			return nil, c.redact(err)
		}
		if int64(len(data)) > max {
			return nil, channel.ErrTooLarge
		}
		return data, nil
	}
}

type update struct {
	UpdateID int64      `json:"update_id"`
	Message  *tgMessage `json:"message"`
}

// Run implements channel.Channel.
func (c *Channel) Run(ctx context.Context, sink channel.Sink) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	if err := c.identify(ctx); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	c.log.Info("telegram bot connected", "username", c.botUser)

	orderer := channel.NewOrderer()
	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		var updates []update
		pctx, cancel := context.WithTimeout(ctx, c.cfg.PollTimeout+15*time.Second)
		err := c.call(pctx, "getUpdates", map[string]any{
			"offset":          offset,
			"timeout":         int(c.cfg.PollTimeout.Seconds()),
			"allowed_updates": []string{"message"},
		}, &updates)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			var ae *apiError
			if errors.As(err, &ae) && (ae.Code == http.StatusUnauthorized || ae.Code == http.StatusNotFound) {
				return fmt.Errorf("telegram rejected the bot token: %w", err)
			}
			c.log.Warn("getUpdates failed", "err", err, "retry_in", backoff)
			sleep(ctx, backoff)
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		for _, u := range updates {
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
			in, ok := c.convert(u.Message)
			if !ok {
				continue
			}
			if u.Message.MediaGroupID != "" && len(in.Attachments) > 0 {
				// The pictures of an album arrive as separate messages: collect
				// them into one, which takes the place of the first.
				key := albumKey{u.Message.Chat.ID, u.Message.MediaGroupID}
				if c.joinAlbum(key, in) {
					continue
				}
				wait, accepted := orderer.Enter(in.ChatID)
				in.Accepted = accepted
				a := c.startAlbum(key, in)
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
					select {
					case <-a.done:
					case <-ctx.Done():
						return
					}
					sink(ctx, c.sealAlbum(key, a))
				}()
				continue
			}
			// Updates are handled concurrently, but a chat's messages must be
			// taken in the order they were sent (a backlog after downtime arrives
			// as one batch): each waits until the one before it was accepted.
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
	}
	return nil
}

// identify fetches the bot's own identity, retrying while the network is down.
func (c *Channel) identify(ctx context.Context) error {
	backoff := time.Second
	for {
		var me tgUser
		err := c.call(ctx, "getMe", struct{}{}, &me)
		if err == nil {
			c.botID, c.botUser = me.ID, me.Username
			return nil
		}
		var ae *apiError
		if errors.As(err, &ae) && (ae.Code == http.StatusUnauthorized || ae.Code == http.StatusNotFound) {
			return fmt.Errorf("telegram rejected the bot token: %w", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c.log.Warn("getMe failed", "err", err, "retry_in", backoff)
		sleep(ctx, backoff)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// convert turns a Telegram message into an Incoming. It returns false for
// messages the bot must ignore (channel posts, commands for other bots, ...).
func (c *Channel) convert(m *tgMessage) (channel.Incoming, bool) {
	if m == nil || m.From == nil || m.From.IsBot {
		return channel.Incoming{}, false
	}
	var isGroup bool
	switch m.Chat.Type {
	case "private":
	case "group", "supergroup":
		isGroup = true
	default:
		return channel.Incoming{}, false
	}

	text := m.Text
	if text == "" {
		text = m.Caption
	}
	addressed := !isGroup
	if m.ReplyToMessage != nil && m.ReplyToMessage.From != nil && m.ReplyToMessage.From.ID == c.botID {
		addressed = true
	}

	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "/") {
		fields := strings.Fields(text)
		cmd := fields[0]
		if at := strings.IndexByte(cmd, '@'); at > 0 {
			if !strings.EqualFold(cmd[at+1:], c.botUser) {
				return channel.Incoming{}, false // command for another bot
			}
			text = strings.TrimSpace(cmd[:at] + " " + strings.Join(fields[1:], " "))
			addressed = true // it names this bot
		}
		if !c.cfg.NoCommands {
			addressed = true
		}
	}
	if c.botUser != "" {
		re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(c.botUser) + `\b`)
		if re.MatchString(text) {
			addressed = true
			text = strings.TrimSpace(re.ReplaceAllString(text, ""))
		}
	}

	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" {
		name = m.From.Username
	}
	if name == "" {
		name = "user"
	}
	thread := int64(0)
	if m.IsTopicMessage {
		thread = m.MessageThreadID
	}
	var sentAt time.Time
	if m.Date > 0 {
		sentAt = time.Unix(m.Date, 0)
	}
	atts := c.attachments(m)
	return channel.Incoming{
		Attachments:   atts,
		ReceivedAt:    sentAt,
		Channel:       "telegram",
		ChatID:        strconv.FormatInt(m.Chat.ID, 10),
		UserID:        strconv.FormatInt(m.From.ID, 10),
		UserName:      name,
		Text:          text,
		IsGroup:       isGroup,
		Addressed:     addressed,
		HasAttachment: m.hasMedia() && len(atts) == 0,
		Responder: &responder{
			c: c, chatID: m.Chat.ID, replyTo: m.MessageID, thread: thread, isGroup: isGroup,
		},
	}, true
}

type responder struct {
	c       *Channel
	chatID  int64
	replyTo int64
	thread  int64
	isGroup bool
}

func (r *responder) Typing(ctx context.Context) error {
	p := map[string]any{"chat_id": r.chatID, "action": "typing"}
	if r.thread != 0 {
		p["message_thread_id"] = r.thread
	}
	return r.c.call(ctx, "sendChatAction", p, nil)
}

func (r *responder) Send(ctx context.Context, text string) error {
	for i, part := range channel.Split(text, maxMessageRunes) {
		p := map[string]any{"chat_id": r.chatID, "text": part}
		if r.thread != 0 {
			p["message_thread_id"] = r.thread
		}
		if i == 0 && r.isGroup {
			p["reply_parameters"] = map[string]any{"message_id": r.replyTo, "allow_sending_without_reply": true}
		}
		if err := r.sendWithRetry(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

// sendWithRetry honours Telegram's flood-control retry_after once.
func (r *responder) sendWithRetry(ctx context.Context, p map[string]any) error {
	err := r.c.call(ctx, "sendMessage", p, nil)
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == http.StatusTooManyRequests && ae.RetryAfter > 0 && ae.RetryAfter <= 60 {
		sleep(ctx, time.Duration(ae.RetryAfter)*time.Second)
		return r.c.call(ctx, "sendMessage", p, nil)
	}
	return err
}
