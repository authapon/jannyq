package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/authapon/jannyq/internal/ntfy"
	"github.com/authapon/jannyq/internal/trigger"
)

// TriggerTools are the tools with which the model manages the scheduled
// reminders and tasks of a chat: trigger_create, trigger_list, trigger_update
// and trigger_delete.
type TriggerTools struct {
	Store *trigger.Store
	// Location is the time zone schedules are read in unless the user names
	// another (the bot's --timezone).
	Location *time.Location
	// CanSend says whether a channel can send on its own; Windows holds, per
	// channel, how long after a person's last message the bot may still write
	// to them (Messenger and WhatsApp: 24 hours).
	CanSend func(channel string) bool
	Windows map[string]time.Duration
	// MaxPerChat limits the triggers of a chat (default 20). MinTaskInterval is
	// the shortest time between two runs of a task (default 15 minutes); a
	// reminder may repeat every minute. NoTasks refuses tasks (only reminders).
	MaxPerChat      int
	MinTaskInterval time.Duration
	NoTasks         bool
	// Ntfy, when set, lets users have scheduled messages pushed through ntfy:
	// the notify, ntfy_topic and priority arguments and the ntfy_settings tool
	// exist only then.
	Ntfy *ntfy.Client
	// Wake tells the scheduler that triggers changed.
	Wake func()
	Log  *slog.Logger
	Now  func() time.Time
}

const (
	maxTriggerText  = 2000
	maxTriggerTitle = 80
	maxAhead        = 2 * 365 * 24 * time.Hour
)

// Tools returns the four tools (and ntfy_settings when ntfy is set up).
func (tt *TriggerTools) Tools() []Tool {
	list := []Tool{triggerCreate{tt}, triggerList{tt}, triggerUpdate{tt}, triggerDelete{tt}}
	if tt.Ntfy != nil {
		list = append(list, ntfySettings{tt})
	}
	return list
}

func (tt *TriggerTools) now() time.Time {
	if tt.Now != nil {
		return tt.Now()
	}
	return time.Now()
}

func (tt *TriggerTools) loc() *time.Location {
	if tt.Location != nil {
		return tt.Location
	}
	return time.Local
}

func (tt *TriggerTools) maxPerChat() int {
	if tt.MaxPerChat > 0 {
		return tt.MaxPerChat
	}
	return 20
}

func (tt *TriggerTools) minTask() time.Duration {
	if tt.MinTaskInterval > 0 {
		return tt.MinTaskInterval
	}
	return 15 * time.Minute
}

func (tt *TriggerTools) logf(msg string, cc CallContext, t trigger.Trigger) {
	if tt.Log == nil {
		return
	}
	kind := "once"
	if t.Recurring() {
		kind = "cron"
	}
	tt.Log.Info(msg, "id", t.ID, "mode", t.Mode, "kind", kind, "channel", cc.Channel, "chat", cc.ChatID, "user", cc.UserID,
		"next", formatTime(t.Next, t.Location()), "status", t.Status)
}

func (tt *TriggerTools) wake() {
	if tt.Wake != nil {
		tt.Wake()
	}
}

// schedule is the "when" part of the arguments of create and update.
type schedule struct {
	At       string `json:"at"`
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
}

func (s schedule) given() bool { return s.At != "" || s.Cron != "" }

const scheduleHelp = "one of: \"at\" (a single date and time, ISO 8601, e.g. 2026-10-09T15:00:00+07:00, or without the offset to read it in \"timezone\") " +
	"or \"cron\" (a repeating schedule: five fields minute hour day-of-month month day-of-week, e.g. \"0 7 * * *\" every day at 07:00, " +
	"\"30 9 * * mon-fri\" weekdays 09:30, \"*/30 * * * *\" every 30 minutes, \"0 8 1 * *\" the 1st of each month; or @daily, @hourly, @weekly, @monthly)"

// resolve turns a schedule into what is stored, checking it against the rules.
func (tt *TriggerTools) resolve(cc CallContext, mode, notify string, s schedule, current *trigger.Trigger) (cron string, at, next time.Time, zone string, err error) {
	if s.At != "" && s.Cron != "" {
		return "", time.Time{}, time.Time{}, "", errors.New(`give either "at" (once) or "cron" (repeating), not both`)
	}
	now := tt.now()
	loc := tt.loc()
	zone = loc.String()
	if current != nil && current.Zone != "" {
		zone = current.Zone
	}
	if s.Timezone != "" {
		l, lerr := time.LoadLocation(strings.TrimSpace(s.Timezone))
		if lerr != nil {
			return "", time.Time{}, time.Time{}, "", fmt.Errorf("unknown time zone %q (use a name such as Asia/Bangkok or Europe/London)", s.Timezone)
		}
		loc, zone = l, l.String()
	} else if current != nil && current.Zone != "" {
		if l, lerr := time.LoadLocation(current.Zone); lerr == nil {
			loc = l
		}
	}
	// The limit of the 24 hour window applies to what goes to the chat; a
	// message for ntfy only is not held back by it.
	window := tt.Windows[cc.Channel]
	if notify == trigger.NotifyNtfy {
		window = 0
	}

	switch {
	case s.Cron != "":
		c, perr := trigger.ParseCron(s.Cron)
		if perr != nil {
			return "", time.Time{}, time.Time{}, "", perr
		}
		if window > 0 {
			return "", time.Time{}, time.Time{}, "", fmt.Errorf("%s only lets the bot write to someone within %s of their last message, so repeating reminders are not possible there; "+
				"set a single reminder (\"at\") for the next %s instead%s", cc.Channel, window, window-time.Hour, tt.ntfyWindowHint())
		}
		n, ok := c.Next(now, loc)
		if !ok {
			return "", time.Time{}, time.Time{}, "", fmt.Errorf("the schedule %q never comes", s.Cron)
		}
		if err := tt.checkGap(c, mode, now, loc); err != nil {
			return "", time.Time{}, time.Time{}, "", err
		}
		return strings.TrimSpace(s.Cron), time.Time{}, n, zone, nil
	case s.At != "":
		t, perr := parseAt(s.At, loc)
		if perr != nil {
			return "", time.Time{}, time.Time{}, "", perr
		}
		if !t.After(now) {
			return "", time.Time{}, time.Time{}, "", fmt.Errorf("%s has already passed (it is now %s); ask the user for a time in the future", formatTime(t, loc), formatTime(now, loc))
		}
		if t.Sub(now) > maxAhead {
			return "", time.Time{}, time.Time{}, "", errors.New("that is more than two years away; schedules are limited to two years ahead")
		}
		if window > 0 && t.Sub(now) > window-time.Hour {
			return "", time.Time{}, time.Time{}, "", fmt.Errorf("%s only lets the bot write to someone within %s of their last message, so a reminder can be set only for the next %s; "+
				"tell the user that, or ask for an earlier time%s", cc.Channel, window, window-time.Hour, tt.ntfyWindowHint())
		}
		return "", t, t, zone, nil
	case current != nil: // only the time zone changed: read the same schedule in it
		if current.Recurring() {
			c, _ := trigger.ParseCron(current.Cron)
			n, ok := c.Next(now, loc)
			if !ok {
				return "", time.Time{}, time.Time{}, "", errors.New("the schedule never comes")
			}
			if err := tt.checkGap(c, mode, now, loc); err != nil {
				return "", time.Time{}, time.Time{}, "", err
			}
			if window > 0 {
				return "", time.Time{}, time.Time{}, "", fmt.Errorf("%s only lets the bot write to someone within %s of their last message, so repeating reminders are not possible there "+
					"(unless they are sent through ntfy only)", cc.Channel, window)
			}
			return current.Cron, time.Time{}, n, zone, nil
		}
		if window > 0 && current.At.Sub(now) > window-time.Hour {
			return "", time.Time{}, time.Time{}, "", fmt.Errorf("%s only lets the bot write to someone within %s of their last message, so that time is too far away "+
				"(unless the reminder is sent through ntfy only)", cc.Channel, window)
		}
		return "", current.At, current.At, zone, nil
	}
	return "", time.Time{}, time.Time{}, "", fmt.Errorf("say when: %s", scheduleHelp)
}

// checkGap refuses a repeating schedule that runs too often for its mode.
func (tt *TriggerTools) checkGap(c *trigger.Cron, mode string, now time.Time, loc *time.Location) error {
	min, what := time.Minute, "a reminder"
	if mode == trigger.ModeTask {
		min, what = tt.minTask(), "a task"
	}
	if gap := c.MinGap(now, loc, 30); gap < min {
		return fmt.Errorf("that is too often: %s may repeat at most every %s (this repeats every %s)", what, min, gap)
	}
	return nil
}

// parseAt reads a date and time: ISO 8601 with an offset, or without one in loc.
func parseAt(s string, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot read the time %q: write it as 2026-10-09T15:00:00+07:00", s)
}

func formatTime(t time.Time, loc *time.Location) string {
	if t.IsZero() {
		return "-"
	}
	return t.In(loc).Format("Mon 2006-01-02 15:04 MST (-07:00)")
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// describe says what a trigger is, for the model to pass on to the user.
func describe(t trigger.Trigger, now time.Time, withText bool, viewer CallContext) string {
	loc := t.Location()
	var when string
	if t.Recurring() {
		when = fmt.Sprintf("repeats by cron %q in %s", t.Cron, t.Zone)
	} else {
		when = "once"
	}
	var sb strings.Builder
	title := t.Title
	if title == "" {
		title = clip(t.Text, 40)
	}
	fmt.Fprintf(&sb, "#%d [%s, %s] %q — %s", t.ID, t.Mode, t.Status, title, when)
	switch {
	case t.Status == trigger.StatusActive && !t.Next.IsZero():
		fmt.Fprintf(&sb, "; next: %s", formatTime(t.Next, loc))
		if t.Recurring() {
			if c, err := trigger.ParseCron(t.Cron); err == nil {
				var more []string
				at := t.Next
				for i := 0; i < 2; i++ {
					n, ok := c.Next(at, loc)
					if !ok {
						break
					}
					more = append(more, formatTime(n, loc))
					at = n
				}
				if len(more) > 0 {
					fmt.Fprintf(&sb, ", then %s", strings.Join(more, ", "))
				}
			}
		}
	case t.Status == trigger.StatusDone:
		sb.WriteString("; already done")
	case t.Status == trigger.StatusDisabled:
		sb.WriteString("; switched off by the bot: " + t.LastError)
	}
	sb.WriteString(deliveryText(t, viewer))
	fmt.Fprintf(&sb, "; set up by %s", t.OwnerName)
	if t.LastResult != "" {
		fmt.Fprintf(&sb, "; last: %s at %s", t.LastResult, formatTime(t.LastRun, loc))
		if t.LastError != "" && t.Status != trigger.StatusDisabled {
			fmt.Fprintf(&sb, " (%s)", clip(t.LastError, 120))
		}
	}
	if withText {
		fmt.Fprintf(&sb, "\n    text: %s", clip(t.Text, 300))
	}
	return sb.String()
}

func mustOwn(t trigger.Trigger, cc CallContext, verb string) error {
	if t.OwnerID == cc.UserID {
		return nil
	}
	return fmt.Errorf("only %s, who set up trigger #%d, can %s it", t.OwnerName, t.ID, verb)
}

// --- trigger_create ---

type triggerCreate struct{ tt *TriggerTools }

func (triggerCreate) Name() string { return "trigger_create" }

func (triggerCreate) Description() string {
	return "Schedule a reminder or a task for this chat: at a given time once, or repeatedly (cron). " +
		"At the time the assistant itself writes the reminder or does the task and sends the result to this chat. " +
		"The result tells you when it will run: pass that on to the user and check it matches what they asked."
}

func (c triggerCreate) Parameters() []byte {
	return []byte(`{"type":"object","properties":{` + c.tt.ntfyParams(true) +
		`"mode":{"type":"string","enum":["remind","task"],"description":"remind (default): remind the user of something. task: carry out an instruction, with tools such as web search, and report."},` +
		`"text":{"type":"string","description":"For remind: what to remind the user of, with all details (names, numbers, places). For task: a complete, self-contained instruction, as the user would give it now."},` +
		`"title":{"type":"string","description":"A short name for lists (optional)."},` +
		`"at":{"type":"string","description":"A single date and time, ISO 8601, e.g. 2026-10-09T15:00:00+07:00."},` +
		`"cron":{"type":"string","description":"A repeating schedule: five fields minute hour day-of-month month day-of-week, e.g. 0 7 * * *, or @daily, @hourly, @weekly, @monthly."},` +
		`"timezone":{"type":"string","description":"IANA time zone the schedule is read in, e.g. Asia/Bangkok (default: the bot's)."}},` +
		`"required":["text"]}`)
}

// Hint is added to the system prompt.
func (triggerCreate) Hint() string {
	return "Scheduling: when the user asks to be reminded of something at a time, or to have something done regularly (\"remind me at 15:00 to call Peter\", " +
		"\"find interesting news for me every day at 7\"), use trigger_create (mode remind for reminders, task for things to do), trigger_list to show what is scheduled, " +
		"trigger_update to change or pause one and trigger_delete to remove one. Work out the exact date and time from the current time in the message headers " +
		"(and say it back to the user with the weekday so that a mistake shows); for a single time use \"at\", for \"every day\" and the like use \"cron\" " +
		"(0 7 * * * = every day at 07:00; 30 9 * * mon-fri = weekdays at 09:30). If the time is unclear, ask. " +
		"A task's text must be complete in itself: it will be carried out later, when the user is not there to explain."
}

func (c triggerCreate) Execute(ctx context.Context, cc CallContext, args []byte) (string, error) {
	var in struct {
		Mode  string `json:"mode"`
		Text  string `json:"text"`
		Title string `json:"title"`
		deliveryArgs
		schedule
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	tt := c.tt
	if cc.ChatID == "" || cc.Channel == "" {
		return "", errors.New("there is no chat to send to")
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	switch mode {
	case "", trigger.ModeRemind:
		mode = trigger.ModeRemind
	case trigger.ModeTask:
		if tt.NoTasks {
			return "", errors.New("scheduled tasks are switched off here; only reminders can be set")
		}
	default:
		return "", fmt.Errorf("mode must be remind or task, not %q", in.Mode)
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return "", errors.New("text is empty: say what to remind the user of, or what to do")
	}
	if utf8.RuneCountInString(text) > maxTriggerText {
		return "", fmt.Errorf("text is too long (at most %d characters)", maxTriggerText)
	}
	n, err := tt.Store.Count(ctx, cc.Channel, cc.ChatID)
	if err != nil {
		return "", err
	}
	if n >= tt.maxPerChat() {
		return "", fmt.Errorf("this chat already has %d scheduled items (the limit); delete one first", n)
	}
	t := trigger.Trigger{
		Channel: cc.Channel, ChatID: cc.ChatID, IsGroup: cc.IsGroup, OwnerID: cc.UserID, OwnerName: nameOf(cc),
		Mode: mode, Title: clip(strings.TrimSpace(in.Title), maxTriggerTitle), Text: text,
		Status: trigger.StatusActive, Created: tt.now(), Notify: trigger.NotifyChat,
	}
	if err := tt.applyDelivery(ctx, cc, &t, in.deliveryArgs, true); err != nil {
		return "", err
	}
	if t.Notify != trigger.NotifyNtfy && tt.CanSend != nil && !tt.CanSend(cc.Channel) {
		return "", fmt.Errorf("the %s channel cannot send scheduled messages", cc.Channel)
	}
	cron, at, next, zone, err := tt.resolve(cc, mode, t.Notify, in.schedule, nil)
	if err != nil {
		return "", err
	}
	t.Cron, t.At, t.Next, t.Zone = cron, at, next, zone
	id, err := tt.Store.Create(ctx, t)
	if err != nil {
		return "", err
	}
	t.ID = id
	tt.logf("trigger created", cc, t)
	tt.wake()
	return "Scheduled: " + describe(t, tt.now(), false, cc) + tt.deliveryAdvice(t, cc) + "\nTell the user, in their language, what you set and when it will happen.", nil
}

func nameOf(cc CallContext) string {
	if n := strings.TrimSpace(cc.UserName); n != "" {
		return n
	}
	return "the user"
}

// --- trigger_list ---

type triggerList struct{ tt *TriggerTools }

func (triggerList) Name() string { return "trigger_list" }

func (triggerList) Description() string {
	return "List the reminders and tasks scheduled for this chat, with their numbers, when they run next and what happened last time."
}

func (triggerList) Parameters() []byte { return []byte(`{"type":"object","properties":{}}`) }

func (l triggerList) Execute(ctx context.Context, cc CallContext, _ []byte) (string, error) {
	list, err := l.tt.Store.List(ctx, cc.Channel, cc.ChatID)
	if err != nil {
		return "", err
	}
	now := l.tt.now()
	if len(list) == 0 {
		return "Nothing is scheduled for this chat.", nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d scheduled item(s) in this chat (now: %s):\n", len(list), formatTime(now, l.tt.loc()))
	for _, t := range list {
		sb.WriteString("- " + describe(t, now, true, cc) + "\n")
	}
	return sb.String(), nil
}

// --- trigger_update ---

type triggerUpdate struct{ tt *TriggerTools }

func (triggerUpdate) Name() string { return "trigger_update" }

func (triggerUpdate) Description() string {
	return "Change a scheduled reminder or task of this chat (only the person who set it up can): its text, title, time (at or cron), time zone or mode, " +
		"or pause it with enabled=false and resume it with enabled=true. Give only what changes."
}

func (u triggerUpdate) Parameters() []byte {
	return []byte(`{"type":"object","properties":{` + u.tt.ntfyParams(false) +
		`"id":{"type":"integer","description":"Number of the trigger, from trigger_list."},` +
		`"text":{"type":"string"},"title":{"type":"string"},` +
		`"mode":{"type":"string","enum":["remind","task"]},` +
		`"at":{"type":"string","description":"New single date and time, ISO 8601 (makes it a one-time trigger)."},` +
		`"cron":{"type":"string","description":"New repeating schedule (makes it a repeating trigger)."},` +
		`"timezone":{"type":"string"},` +
		`"enabled":{"type":"boolean","description":"false pauses it, true resumes it."}},` +
		`"required":["id"]}`)
}

func (u triggerUpdate) Execute(ctx context.Context, cc CallContext, args []byte) (string, error) {
	var in struct {
		ID      int64   `json:"id"`
		Text    *string `json:"text"`
		Title   *string `json:"title"`
		Mode    *string `json:"mode"`
		Enabled *bool   `json:"enabled"`
		deliveryArgs
		schedule
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	tt := u.tt
	t, err := tt.Store.Get(ctx, cc.Channel, cc.ChatID, in.ID)
	if errors.Is(err, trigger.ErrNotFound) {
		return "", fmt.Errorf("there is no trigger #%d in this chat (trigger_list shows them)", in.ID)
	}
	if err != nil {
		return "", err
	}
	if err := mustOwn(t, cc, "change"); err != nil {
		return "", err
	}
	if t.Status == trigger.StatusDone && !in.given() {
		return "", fmt.Errorf("trigger #%d has already run; give a new time (at or cron) to schedule it again", t.ID)
	}
	if in.Text != nil {
		s := strings.TrimSpace(*in.Text)
		if s == "" || utf8.RuneCountInString(s) > maxTriggerText {
			return "", fmt.Errorf("text must be 1 to %d characters", maxTriggerText)
		}
		t.Text = s
	}
	if in.Title != nil {
		t.Title = clip(strings.TrimSpace(*in.Title), maxTriggerTitle)
	}
	if in.Mode != nil {
		m := strings.ToLower(strings.TrimSpace(*in.Mode))
		if m != trigger.ModeRemind && m != trigger.ModeTask {
			return "", fmt.Errorf("mode must be remind or task, not %q", *in.Mode)
		}
		if m == trigger.ModeTask && tt.NoTasks {
			return "", errors.New("scheduled tasks are switched off here; only reminders can be set")
		}
		t.Mode = m
	}
	oldNotify := t.Notify
	if err := tt.applyDelivery(ctx, cc, &t, in.deliveryArgs, false); err != nil {
		return "", err
	}
	if t.Notify != trigger.NotifyNtfy && tt.CanSend != nil && !tt.CanSend(cc.Channel) {
		return "", fmt.Errorf("the %s channel cannot send scheduled messages", cc.Channel)
	}
	reschedule := in.given() || in.Timezone != ""
	if t.Notify != oldNotify && t.Status == trigger.StatusActive && tt.Windows[cc.Channel] > 0 {
		reschedule = true // the 24 hour window applies to what goes to the chat
	}
	if t.Status == trigger.StatusDone && in.given() {
		t.Status = trigger.StatusActive // a new time schedules a finished trigger again
	}
	if in.Enabled != nil {
		if *in.Enabled {
			if t.Status == trigger.StatusPaused || t.Status == trigger.StatusDisabled || t.Status == trigger.StatusDone {
				t.Status = trigger.StatusActive
				reschedule = true
			}
		} else {
			t.Status = trigger.StatusPaused
		}
	}
	if in.Mode != nil && t.Status == trigger.StatusActive {
		reschedule = true // the minimum interval depends on the mode
	}
	if reschedule {
		cron, at, next, zone, err := tt.resolve(cc, t.Mode, t.Notify, in.schedule, &t)
		if err != nil {
			return "", err
		}
		t.Cron, t.At, t.Next, t.Zone = cron, at, next, zone
	}
	if err := tt.Store.Update(ctx, t); err != nil {
		return "", err
	}
	tt.logf("trigger updated", cc, t)
	tt.wake()
	return "Updated: " + describe(t, tt.now(), false, cc) + tt.deliveryAdvice(t, cc) + "\nTell the user what changed and when it will run.", nil
}

// --- trigger_delete ---

type triggerDelete struct{ tt *TriggerTools }

func (triggerDelete) Name() string { return "trigger_delete" }

func (triggerDelete) Description() string {
	return "Delete a scheduled reminder or task of this chat (only the person who set it up can)."
}

func (triggerDelete) Parameters() []byte {
	return []byte(`{"type":"object","properties":{"id":{"type":"integer","description":"Number of the trigger, from trigger_list."}},"required":["id"]}`)
}

func (d triggerDelete) Execute(ctx context.Context, cc CallContext, args []byte) (string, error) {
	var in struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	t, err := d.tt.Store.Get(ctx, cc.Channel, cc.ChatID, in.ID)
	if errors.Is(err, trigger.ErrNotFound) {
		return "", fmt.Errorf("there is no trigger #%d in this chat (trigger_list shows them)", in.ID)
	}
	if err != nil {
		return "", err
	}
	if err := mustOwn(t, cc, "delete"); err != nil {
		return "", err
	}
	if err := d.tt.Store.Delete(ctx, cc.Channel, cc.ChatID, in.ID); err != nil {
		return "", err
	}
	d.tt.logf("trigger deleted", cc, t)
	d.tt.wake()
	title := t.Title
	if title == "" {
		title = clip(t.Text, 40)
	}
	return fmt.Sprintf("Deleted trigger #%d (%q). It will not run any more.", t.ID, title), nil
}
