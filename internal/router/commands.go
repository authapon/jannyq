package router

import (
	"context"
	"errors"
	"strings"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/session"
)

// parseCommand extracts a known chat command (e.g. "/reset") from text.
func parseCommand(text string) (string, bool) {
	if !strings.HasPrefix(text, "/") {
		return "", false
	}
	first := strings.ToLower(strings.Fields(text)[0])
	switch first {
	case "/start", "/help", "/reset", "/compact":
		return first[1:], true
	}
	return "", false // unknown "/foo" is treated as ordinary text
}

// command runs a chat command and reports whether it consumed the message.
func (r *Router) command(ctx context.Context, in channel.Incoming, cmd string) bool {
	switch cmd {
	case "start", "help":
		r.say(ctx, in, r.tr.T("help"))
	case "reset":
		err := r.sessions.With(ctx, in.Channel, in.ChatID, func(s *session.Session) error {
			return s.Reset(ctx)
		})
		r.say(ctx, in, r.commandResult(err, "reset_done", in))
	case "compact":
		changed := false
		err := r.sessions.With(ctx, in.Channel, in.ChatID, func(s *session.Session) error {
			select {
			case r.sem <- struct{}{}:
				defer func() { <-r.sem }()
			case <-ctx.Done():
				return ctx.Err()
			}
			var err error
			changed, err = r.agent.Compact(ctx, s, 4)
			return err
		})
		key := "compact_done"
		if err == nil && !changed {
			key = "compact_nothing"
		}
		r.say(ctx, in, r.commandResult(err, key, in))
	}
	return true
}

func (r *Router) commandResult(err error, okKey string, in channel.Incoming) string {
	switch {
	case err == nil:
		return r.tr.T(okKey)
	case errors.Is(err, session.ErrBusy):
		return r.tr.T("busy")
	default:
		r.log.Error("command failed", "channel", in.Channel, "chat", in.ChatID, "err", err)
		return r.tr.T("error_generic")
	}
}
