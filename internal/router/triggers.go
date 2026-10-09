package router

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/authapon/jannyq/internal/agent"
	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/llm"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/trigger"
)

const (
	defaultRemindTimeout = 30 * time.Second
	defaultRemindHistory = 10
	busyRetry            = 20 * time.Second
	lateAfter            = 5 * time.Minute // from this lateness on, the message says so
)

// SetNotifiers gives the router the channels that can send on their own, by
// channel name; scheduled triggers are delivered through them.
func (r *Router) SetNotifiers(n map[string]channel.Notifier) { r.notifiers = n }

// RunTrigger carries out a trigger that has come due: the model is asked to
// give the reminder (or do the task) as part of the owner's chat, and what it
// says is sent to the chat. It implements trigger.Runner (via
// trigger.RunnerFunc).
func (r *Router) RunTrigger(ctx context.Context, t trigger.Trigger, late time.Duration) error {
	owner := channel.Incoming{Channel: t.Channel, ChatID: t.ChatID, UserID: t.OwnerID, UserName: t.OwnerName, IsGroup: t.IsGroup}
	if !r.isAllowed(owner) {
		return &trigger.DisableError{Reason: "the person who set it up may not use the bot any more"}
	}
	n, ok := r.notifiers[t.Channel]
	if !ok {
		return fmt.Errorf("the %s channel is not running, so it cannot send", t.Channel)
	}
	resp, err := n.ResponderFor(t.ChatID, t.IsGroup)
	if err != nil {
		return err
	}
	err = r.sessions.With(ctx, t.Channel, t.ChatID, func(s *session.Session) error {
		return r.fire(ctx, s, t, late, resp)
	})
	if errors.Is(err, session.ErrBusy) {
		return &trigger.RetryError{After: busyRetry}
	}
	return err
}

// fire runs inside the chat's exclusive session lock.
func (r *Router) fire(ctx context.Context, s *session.Session, t trigger.Trigger, late time.Duration, resp channel.Responder) error {
	loc := t.Location()
	now := time.Now()
	if t.Mode == trigger.ModeRemind && r.cfg.TriggerPlain {
		return r.sendPlain(ctx, s, t, late, resp)
	}
	kind := "reminder"
	if t.Mode == trigger.ModeTask {
		kind = "task"
	}
	in := agent.Input{
		Text:    fmt.Sprintf("[Scheduled %s #%d, set up by %s] %s", kind, t.ID, t.OwnerName, t.Text),
		Sender:  t.OwnerName,
		IsGroup: t.IsGroup,
		Channel: t.Channel,
		UserID:  t.OwnerID,
		SentAt:  now,
	}
	id, err := r.agent.Record(ctx, s, in)
	if err != nil {
		return err
	}
	in.AnswerFor = id

	dueAt := t.Next
	if !t.Recurring() && !t.At.IsZero() {
		dueAt = t.At
	}
	lateText := ""
	if late >= lateAfter {
		lateText = fmt.Sprintf(" It is late: it was due at %s but the bot was not running then. Say so briefly, once.",
			dueAt.In(loc).Format("2006-01-02 15:04"))
	}
	timeout := r.cfg.RequestTimeout
	if t.Mode == trigger.ModeTask {
		in.Note = fmt.Sprintf("Note from the system (not from a user): a scheduled task that %s set up earlier has come due now (%s). "+
			"The task: %q. Carry it out now, using your tools where they help, and give the result to them as a message: "+
			"complete but concise, in the language of the conversation. Do not ask whether to do it; if you cannot, say what is missing.%s",
			t.OwnerName, now.In(loc).Format(time.RFC3339), t.Text, lateText)
	} else {
		in.NoTools = true
		in.HistoryLimit = r.cfg.TriggerHistory
		if in.HistoryLimit <= 0 {
			in.HistoryLimit = defaultRemindHistory
		}
		timeout = r.cfg.TriggerRemindTimeout
		if timeout <= 0 {
			timeout = defaultRemindTimeout
		}
		in.Note = fmt.Sprintf("Note from the system (not from a user): a reminder that %s asked you to give has come due now (%s). "+
			"Their reminder text: %q. Give the reminder yourself, as the assistant of this chat: short, natural and friendly, in the language of the conversation, "+
			"addressing %s by name. Keep every detail of the text exactly (names, numbers, times, places) and add no new facts. Do not call tools.%s",
			t.OwnerName, now.In(loc).Format(time.RFC3339), t.Text, t.OwnerName, lateText)
	}

	reply, err := r.askForTrigger(ctx, s, in, timeout)
	if t.Mode == trigger.ModeRemind && (err != nil || strings.TrimSpace(reply) == "") {
		// The reminder must not be lost because the model is slow or down: say it plainly.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		r.log.Warn("the model could not give the reminder; sending it as it was written", "trigger", t.ID, "channel", t.Channel, "err", err)
		return r.sendPlain(ctx, s, t, late, resp)
	}
	if err != nil {
		if ctx.Err() == nil {
			title := t.Title
			if title == "" {
				title = t.Text
			}
			if rs := []rune(title); len(rs) > 60 {
				title = string(rs[:60]) + "…"
			}
			_ = resp.Send(ctx, r.tr.T("trigger_task_failed", title))
		}
		return err
	}
	if err := resp.Send(ctx, reply); err != nil {
		return fmt.Errorf("could not deliver the message: %w", err)
	}
	if use, ok := r.agent.ContextUse(t.Channel + ":" + t.ChatID); ok {
		r.log.Info("scheduled message delivered", "trigger", t.ID, "channel", t.Channel, "chat", t.ChatID, "context", use.String())
	}
	return nil
}

// askForTrigger runs the model for a trigger within the concurrency limit.
func (r *Router) askForTrigger(ctx context.Context, s *session.Session, in agent.Input, timeout time.Duration) (string, error) {
	select {
	case r.sem <- struct{}{}:
		defer func() { <-r.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	reply, err := r.agent.Respond(tctx, s, in)
	result := "ok"
	if err != nil {
		result = "error"
	}
	r.cfg.Metrics.Replies.Inc(s.Channel, result)
	r.cfg.Metrics.RequestSeconds.Since(start, s.Channel)
	return reply, err
}

// sendPlain sends a reminder exactly as it was written, and keeps it in the
// conversation.
func (r *Router) sendPlain(ctx context.Context, s *session.Session, t trigger.Trigger, late time.Duration, resp channel.Responder) error {
	text := r.tr.T("trigger_plain", t.Text)
	if late >= lateAfter {
		due := t.Next
		if !t.Recurring() && !t.At.IsZero() {
			due = t.At
		}
		text = r.tr.T("trigger_plain_late", due.In(t.Location()).Format("2006-01-02 15:04"), t.Text)
	}
	_ = s.Append(ctx, llm.Message{Role: llm.RoleAssistant, Content: text})
	if err := resp.Send(ctx, text); err != nil {
		return fmt.Errorf("could not deliver the message: %w", err)
	}
	return nil
}
