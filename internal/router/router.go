// Package router connects channels to the agent: it applies access control,
// rate limiting and concurrency limits, handles chat commands and delivers
// replies.
package router

import (
	"context"
	"errors"
	"github.com/authapon/jannyq/internal/metrics"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/authapon/jannyq/internal/agent"
	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/i18n"
	"github.com/authapon/jannyq/internal/ratelimit"
	"github.com/authapon/jannyq/internal/session"
)

// Group reply policies.
const (
	GroupReplyMention = "mention" // only when addressed
	GroupReplyAll     = "all"     // every message in groups
)

// What the bot keeps of group conversations.
const (
	GroupContextAll       = "all"       // every message, so the model knows who said what
	GroupContextAddressed = "addressed" // only messages addressed to the bot
)

const (
	maxRecordRunes   = 4000 // longest group message kept when it was not addressed to the bot
	recordsPerMinute = 300  // per chat; beyond it, unaddressed messages are not kept
	compactRetry     = 5 * time.Minute
)

// Config controls routing behaviour.
type Config struct {
	// GroupContext is GroupContextAll (default) or GroupContextAddressed.
	GroupContext string
	// CompactAfter is the message count above which a chat that only collects
	// messages is summarised in the background; 0 disables that.
	CompactAfter   int
	AllowedUsers   []string // "id" or "channel:id"; empty allows everyone
	GroupReply     string
	RateLimit      int // messages per user per minute; 0 = unlimited
	MaxConcurrent  int // simultaneous model runs
	RequestTimeout time.Duration
	// Metrics counts messages, answers and files.
	Metrics metrics.Instruments
}

// Router handles incoming messages from all channels.
type Router struct {
	cfg      Config
	sessions *session.Manager
	agent    *agent.Agent
	tr       *i18n.Translator
	log      *slog.Logger

	allowed map[string]bool
	sem     chan struct{}
	limiter *ratelimit.Limiter

	resetWorkspace func(ctx context.Context, workspace string) error
	attach         *attachState

	recordLimiter *ratelimit.Limiter
	wg            sync.WaitGroup
	mu            sync.Mutex
	compactAt     map[string]time.Time
}

// New creates a Router.
func New(cfg Config, sessions *session.Manager, a *agent.Agent, tr *i18n.Translator, log *slog.Logger) *Router {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Minute
	}
	if cfg.GroupReply == "" {
		cfg.GroupReply = GroupReplyMention
	}
	if cfg.GroupContext == "" {
		cfg.GroupContext = GroupContextAll
	}
	if log == nil {
		log = slog.Default()
	}
	r := &Router{
		cfg: cfg, sessions: sessions, agent: a, tr: tr, log: log,
		allowed: map[string]bool{},
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		limiter: ratelimit.New(cfg.RateLimit, time.Minute),

		recordLimiter: ratelimit.New(recordsPerMinute, time.Minute),
		compactAt:     map[string]time.Time{},
	}
	for _, u := range cfg.AllowedUsers {
		if u = strings.TrimSpace(u); u != "" {
			r.allowed[strings.ToLower(u)] = true
		}
	}
	return r
}

// SetWorkspaceReset registers a function that deletes a chat's command
// sandbox workspace; it is called by /reset.
func (r *Router) SetWorkspaceReset(fn func(ctx context.Context, workspace string) error) {
	r.resetWorkspace = fn
}

// Handle processes one incoming message. It is the channel.Sink: it returns
// once the message has been fully handled, but calls in.Accepted as soon as the
// message has been stored (or turned away), before anything slow happens.
func (r *Router) Handle(ctx context.Context, in channel.Incoming) {
	var once sync.Once
	accept := func() {
		once.Do(func() {
			if in.Accepted != nil {
				in.Accepted()
			}
		})
	}
	defer accept()

	if in.Responder == nil {
		return
	}
	if in.ReceivedAt.IsZero() {
		in.ReceivedAt = time.Now()
	}
	r.cfg.Metrics.MessagesReceived.Inc(in.Channel, strconv.FormatBool(!in.IsGroup || in.Addressed))
	if in.IsGroup && !in.Addressed && r.cfg.GroupReply != GroupReplyAll {
		r.record(ctx, in) // not for us, but part of the conversation
		return
	}
	if !r.isAllowed(in) {
		r.cfg.Metrics.MessagesRejected.Inc(in.Channel, "not_allowed")
		accept()
		r.log.Info("message from user that is not allowed", "channel", in.Channel, "user", in.UserID)
		r.say(ctx, in, r.tr.T("not_allowed"))
		return
	}
	if !r.limiter.Allow(in.Channel + ":" + in.UserID) {
		r.cfg.Metrics.MessagesRejected.Inc(in.Channel, "rate_limited")
		accept()
		r.say(ctx, in, r.tr.T("rate_limited"))
		return
	}

	text := strings.TrimSpace(in.Text)
	hasFiles := r.attach != nil && len(in.Attachments) > 0
	if text == "" && !hasFiles {
		accept()
		if in.HasAttachment || len(in.Attachments) > 0 {
			r.say(ctx, in, r.tr.T("unsupported_attachment"))
		}
		return
	}

	if cmd, ok := parseCommand(text); ok && !hasFiles {
		// A reset forgets what was said up to now, not what is said while it waits
		// for its turn: note where "now" is before letting later messages in.
		var upTo int64
		if cmd == "reset" {
			_ = r.sessions.Record(in.Channel, in.ChatID, func(s *session.Session) error {
				upTo, _ = s.LastID(ctx)
				return nil
			})
		}
		accept()
		stopTyping := r.keepTyping(ctx, in.Responder)
		defer stopTyping()
		if r.command(ctx, in, cmd, upTo) {
			return
		}
	}

	// Store the message now, in the order it arrived, without waiting for a
	// request of this chat that may still be running.
	id, err := r.store(ctx, in, text)
	accept()
	if err != nil {
		if ctx.Err() == nil {
			r.log.Error("could not store a message", "channel", in.Channel, "chat", in.ChatID, "err", err)
			r.say(ctx, in, r.tr.T("error_generic"))
		}
		return
	}

	stopTyping := r.keepTyping(ctx, in.Responder)
	defer stopTyping()
	err = r.sessions.With(ctx, in.Channel, in.ChatID, func(s *session.Session) error {
		if hasFiles {
			// Reading files takes a while, so it happens here, after the
			// message has taken its place in the conversation.
			if read := r.ingest(ctx, s, in, id); read == 0 && text == "" {
				return nil // nothing to answer: the user was told what went wrong
			}
		}
		r.respond(ctx, s, in, text, id)
		return nil
	})
	switch {
	case errors.Is(err, session.ErrBusy):
		r.cfg.Metrics.MessagesRejected.Inc(in.Channel, "busy")
		r.say(ctx, in, r.tr.T("busy"))
	case err != nil && ctx.Err() == nil:
		r.log.Error("session error", "channel", in.Channel, "chat", in.ChatID, "err", err)
		r.say(ctx, in, r.tr.T("error_generic"))
	}
}

// store saves an incoming message and returns its id.
func (r *Router) store(ctx context.Context, in channel.Incoming, text string) (int64, error) {
	var id int64
	err := r.sessions.Record(in.Channel, in.ChatID, func(s *session.Session) error {
		var err error
		id, err = r.agent.Record(ctx, s, r.input(in, text))
		return err
	})
	return id, err
}

func (r *Router) input(in channel.Incoming, text string) agent.Input {
	return agent.Input{
		Text:    text,
		Sender:  in.UserName,
		IsGroup: in.IsGroup,
		Channel: in.Channel,
		UserID:  in.UserID,
		Origin:  in.Origin,
		SentAt:  in.ReceivedAt,
	}
}

// respond runs the agent for one stored message and delivers the result. It
// runs inside the chat's exclusive session lock.
func (r *Router) respond(ctx context.Context, s *session.Session, in channel.Incoming, text string, id int64) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	input := r.input(in, text)
	input.AnswerFor = id
	start := time.Now()
	reply, err := r.agent.Respond(reqCtx, s, input)
	result := "ok"
	switch {
	case errors.Is(err, agent.ErrEmptyResponse):
		reply, result = r.tr.T("empty_response"), "empty"
	case err != nil:
		r.log.Error("agent failed", "channel", in.Channel, "chat", in.ChatID, "err", err)
		reply, result = r.tr.T("error_generic"), "error"
	}
	r.cfg.Metrics.Replies.Inc(in.Channel, result)
	r.cfg.Metrics.RequestSeconds.Since(start, in.Channel)
	if err := in.Responder.Send(ctx, reply); err != nil {
		r.log.Error("send failed", "channel", in.Channel, "chat", in.ChatID, "err", err)
	}

	// Compaction runs after the reply so the user is not kept waiting.
	cctx, ccancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer ccancel()
	if _, err := r.agent.MaybeCompact(cctx, s); err != nil && ctx.Err() == nil {
		r.log.Warn("compaction failed", "channel", in.Channel, "chat", in.ChatID, "err", err)
	}
}

func (r *Router) isAllowed(in channel.Incoming) bool {
	if len(r.allowed) == 0 {
		return true
	}
	return r.allowed[strings.ToLower(in.UserID)] ||
		r.allowed[strings.ToLower(in.Channel+":"+in.UserID)]
}

func (r *Router) say(ctx context.Context, in channel.Incoming, text string) {
	if err := in.Responder.Send(ctx, text); err != nil {
		r.log.Error("send failed", "channel", in.Channel, "chat", in.ChatID, "err", err)
	}
}

// keepTyping shows the typing indicator until the returned func is called.
func (r *Router) keepTyping(ctx context.Context, resp channel.Responder) (stop func()) {
	tctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(4 * time.Second)
		defer tick.Stop()
		for {
			_ = resp.Typing(tctx)
			select {
			case <-tctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return func() {
		cancel()
		wg.Wait()
	}
}

// Wait blocks until background work (compaction of collected conversations)
// has finished.
func (r *Router) Wait() { r.wg.Wait() }

// record keeps a group message that was not addressed to the bot, without
// answering it, so that the model later knows what people said to each other.
// It never waits for a running request of the chat: collecting messages must
// not queue up behind a slow model, nor use up the chat's request queue.
func (r *Router) record(ctx context.Context, in channel.Incoming) {
	if r.cfg.GroupContext != GroupContextAll {
		return
	}
	text := strings.TrimSpace(in.Text)
	hasFiles := r.attach != nil && len(in.Attachments) > 0
	if (text == "" && !hasFiles) || !r.isAllowed(in) {
		return
	}
	if !r.recordLimiter.Allow(in.Channel + ":" + in.ChatID) {
		return // a flood: keep what we have rather than the whole stream
	}
	if rs := []rune(text); len(rs) > maxRecordRunes {
		text = string(rs[:maxRecordRunes]) + "…"
	}
	var count int
	err := r.sessions.Record(in.Channel, in.ChatID, func(s *session.Session) error {
		in := in
		in.IsGroup = true
		id, err := r.agent.Record(ctx, s, r.input(in, text))
		if err != nil {
			return err
		}
		if hasFiles {
			r.recordUnread(ctx, s, id, in.Attachments)
		}
		count, _ = s.Count(ctx)
		// Last resort when compaction cannot run (the model is down, say):
		// the chat must not grow without bound.
		if r.cfg.CompactAfter > 0 && count > 5*r.cfg.CompactAfter {
			_, _ = s.Prune(ctx, 3*r.cfg.CompactAfter)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() == nil {
			r.log.Warn("could not keep a group message", "channel", in.Channel, "chat", in.ChatID, "err", err)
		}
		return
	}
	if r.cfg.CompactAfter > 0 && count > r.cfg.CompactAfter {
		r.scheduleCompaction(ctx, in.Channel, in.ChatID)
	}
}

// scheduleCompaction summarises a chat in the background. A chat that
// nobody addresses never reaches the compaction that normally follows an
// answer, so collecting messages has to trigger it, at most every few minutes.
func (r *Router) scheduleCompaction(ctx context.Context, channelName, chatID string) {
	key := channelName + ":" + chatID
	r.mu.Lock()
	if last, ok := r.compactAt[key]; ok && time.Since(last) < compactRetry {
		r.mu.Unlock()
		return
	}
	if len(r.compactAt) > 10000 {
		for k, t := range r.compactAt {
			if time.Since(t) >= compactRetry {
				delete(r.compactAt, k)
			}
		}
	}
	r.compactAt[key] = time.Now()
	r.mu.Unlock()

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		err := r.sessions.With(ctx, channelName, chatID, func(s *session.Session) error {
			select {
			case r.sem <- struct{}{}:
				defer func() { <-r.sem }()
			case <-ctx.Done():
				return ctx.Err()
			}
			cctx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
			defer cancel()
			_, err := r.agent.MaybeCompact(cctx, s)
			return err
		})
		if err != nil && !errors.Is(err, session.ErrBusy) && ctx.Err() == nil {
			r.log.Warn("background compaction failed", "channel", channelName, "chat", chatID, "err", err)
		}
	}()
}
