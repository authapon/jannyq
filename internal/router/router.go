// Package router connects channels to the agent: it applies access control,
// rate limiting and concurrency limits, handles chat commands and delivers
// replies.
package router

import (
	"context"
	"errors"
	"log/slog"
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

// Config controls routing behaviour.
type Config struct {
	AllowedUsers   []string // "id" or "channel:id"; empty allows everyone
	GroupReply     string
	RateLimit      int // messages per user per minute; 0 = unlimited
	MaxConcurrent  int // simultaneous model runs
	RequestTimeout time.Duration
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
	if log == nil {
		log = slog.Default()
	}
	r := &Router{
		cfg: cfg, sessions: sessions, agent: a, tr: tr, log: log,
		allowed: map[string]bool{},
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		limiter: ratelimit.New(cfg.RateLimit, time.Minute),
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

// Handle processes one incoming message. It is the channel.Sink.
func (r *Router) Handle(ctx context.Context, in channel.Incoming) {
	if in.Responder == nil {
		return
	}
	if in.IsGroup && !in.Addressed && r.cfg.GroupReply != GroupReplyAll {
		return
	}
	if !r.isAllowed(in) {
		r.log.Info("message from user that is not allowed", "channel", in.Channel, "user", in.UserID)
		r.say(ctx, in, r.tr.T("not_allowed"))
		return
	}
	if !r.limiter.Allow(in.Channel + ":" + in.UserID) {
		r.say(ctx, in, r.tr.T("rate_limited"))
		return
	}

	text := strings.TrimSpace(in.Text)
	if text == "" {
		if in.HasAttachment {
			r.say(ctx, in, r.tr.T("unsupported_attachment"))
		}
		return
	}

	stopTyping := r.keepTyping(ctx, in.Responder)
	defer stopTyping()

	if cmd, ok := parseCommand(text); ok {
		if r.command(ctx, in, cmd) {
			return
		}
	}

	err := r.sessions.With(ctx, in.Channel, in.ChatID, func(s *session.Session) error {
		r.respond(ctx, s, in, text)
		return nil
	})
	switch {
	case errors.Is(err, session.ErrBusy):
		r.say(ctx, in, r.tr.T("busy"))
	case err != nil && ctx.Err() == nil:
		r.log.Error("session error", "channel", in.Channel, "chat", in.ChatID, "err", err)
		r.say(ctx, in, r.tr.T("error_generic"))
	}
}

// respond runs the agent for one message and delivers the result. It runs
// inside the chat's exclusive session lock.
func (r *Router) respond(ctx context.Context, s *session.Session, in channel.Incoming, text string) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, r.cfg.RequestTimeout)
	defer cancel()
	reply, err := r.agent.Reply(reqCtx, s, agent.Input{
		Text:    text,
		Sender:  in.UserName,
		IsGroup: in.IsGroup,
		Channel: in.Channel,
		UserID:  in.UserID,
		Origin:  in.Origin,
	})
	switch {
	case errors.Is(err, agent.ErrEmptyResponse):
		reply = r.tr.T("empty_response")
	case err != nil:
		r.log.Error("agent failed", "channel", in.Channel, "chat", in.ChatID, "err", err)
		reply = r.tr.T("error_generic")
	}
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
