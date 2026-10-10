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
	"github.com/authapon/jannyq/internal/ntfy"
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

// PrefsFunc looks up what a user has chosen for their scheduled messages (the
// ntfy topic they gave).
type PrefsFunc func(ctx context.Context, channel, userID string) (trigger.Prefs, error)

// SetNtfy gives the router the ntfy server that triggers may push to, and how
// to find a user's own topic. A nil client means ntfy is not set up.
func (r *Router) SetNtfy(c *ntfy.Client, prefs PrefsFunc) {
	r.ntfy, r.prefs = c, prefs
}

// RunTrigger carries out a trigger that has come due: the model is asked to
// give the reminder (or do the task) as part of the owner's chat, and what it
// says is sent to the chat. It implements trigger.Runner (via
// trigger.RunnerFunc).
func (r *Router) RunTrigger(ctx context.Context, t trigger.Trigger, late time.Duration) error {
	owner := channel.Incoming{Channel: t.Channel, ChatID: t.ChatID, UserID: t.OwnerID, UserName: t.OwnerName, IsGroup: t.IsGroup}
	if !r.isAllowed(owner) {
		return &trigger.DisableError{Reason: "the person who set it up may not use the bot any more"}
	}
	// The chat is where a message goes unless it is meant for ntfy only; if
	// the channel cannot send, do not even ask the model.
	respond := func() (channel.Responder, error) {
		n, ok := r.notifiers[t.Channel]
		if !ok {
			return nil, fmt.Errorf("the %s channel is not running, so it cannot send", t.Channel)
		}
		return n.ResponderFor(t.ChatID, t.IsGroup)
	}
	if t.Notify != trigger.NotifyNtfy {
		if _, err := respond(); err != nil {
			return err
		}
	}
	err := r.sessions.With(ctx, t.Channel, t.ChatID, func(s *session.Session) error {
		return r.fire(ctx, s, t, late, respond)
	})
	if errors.Is(err, session.ErrBusy) {
		return &trigger.RetryError{After: busyRetry}
	}
	return err
}

// fire runs inside the chat's exclusive session lock.
func (r *Router) fire(ctx context.Context, s *session.Session, t trigger.Trigger, late time.Duration, respond func() (channel.Responder, error)) error {
	loc := t.Location()
	now := time.Now()
	if t.Mode == trigger.ModeRemind && r.cfg.TriggerPlain {
		return r.sendPlain(ctx, s, t, late, respond)
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
		return r.sendPlain(ctx, s, t, late, respond)
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
			_ = r.deliver(ctx, t, r.tr.T("trigger_task_failed", title), respond)
		}
		return err
	}
	if err := r.deliver(ctx, t, reply, respond); err != nil {
		return err
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
func (r *Router) sendPlain(ctx context.Context, s *session.Session, t trigger.Trigger, late time.Duration, respond func() (channel.Responder, error)) error {
	text := r.tr.T("trigger_plain", t.Text)
	if late >= lateAfter {
		due := t.Next
		if !t.Recurring() && !t.At.IsZero() {
			due = t.At
		}
		text = r.tr.T("trigger_plain_late", due.In(t.Location()).Format("2006-01-02 15:04"), t.Text)
	}
	_ = s.Append(ctx, llm.Message{Role: llm.RoleAssistant, Content: text})
	return r.deliver(ctx, t, text, respond)
}

// deliver sends what a trigger has to say the way its owner chose: in the
// chat, through ntfy, or both. When ntfy fails the chat gets the message, with
// a note, so that it is not lost; when both are wanted, one of them is enough.
func (r *Router) deliver(ctx context.Context, t trigger.Trigger, text string, respond func() (channel.Responder, error)) error {
	toChat := t.Notify != trigger.NotifyNtfy
	pushed := false
	note := ""
	if t.Notify == trigger.NotifyNtfy || t.Notify == trigger.NotifyBoth {
		err := r.pushNtfy(ctx, t, text)
		switch {
		case err == nil:
			pushed = true
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			note = r.tr.T("trigger_ntfy_failed", err.Error())
			toChat = true
			r.log.Warn("could not push the scheduled message through ntfy", "trigger", t.ID, "channel", t.Channel, "chat", t.ChatID, "err", err)
		}
	}
	if !toChat {
		return nil
	}
	if note != "" {
		text += "\n\n" + note
	}
	resp, err := respond()
	if err == nil {
		err = resp.Send(ctx, text)
	}
	if err != nil {
		if pushed {
			// ntfy has it; the chat (a closed 24 h window, say) may be unreachable
			r.log.Warn("the scheduled message reached ntfy but not the chat", "trigger", t.ID, "channel", t.Channel, "chat", t.ChatID, "err", err)
			return nil
		}
		return fmt.Errorf("could not deliver the message: %w", err)
	}
	return nil
}

// pushNtfy sends a trigger's message to its ntfy topic: the one the trigger
// names, or else the owner's own.
func (r *Router) pushNtfy(ctx context.Context, t trigger.Trigger, text string) error {
	if r.ntfy == nil {
		return errors.New("ntfy is not set up on this bot")
	}
	topic := t.NtfyTopic
	if topic == "" && r.prefs != nil {
		p, err := r.prefs(ctx, t.Channel, t.OwnerID)
		if err != nil {
			return errors.New("could not look up the ntfy topic")
		}
		topic = p.NtfyTopic
	}
	if topic == "" {
		return errors.New("no ntfy topic is set")
	}
	title, tag := t.Title, "alarm_clock"
	if t.Mode == trigger.ModeTask {
		tag = "robot"
		if title == "" {
			title = r.tr.T("trigger_ntfy_title_task")
		}
	} else if title == "" {
		title = r.tr.T("trigger_ntfy_title_remind")
	}
	err := r.ntfy.Publish(ctx, ntfy.Message{Topic: topic, Title: title, Body: text, Priority: t.Priority, Tags: []string{tag}})
	result := "ok"
	if err != nil {
		result = "error"
	}
	r.cfg.Metrics.Ntfy.Inc(result)
	if err == nil {
		r.log.Info("scheduled message pushed through ntfy", "trigger", t.ID, "channel", t.Channel, "topic", ntfy.Short(topic))
	}
	return err
}
