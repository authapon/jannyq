package tool

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/authapon/jannyq/internal/trigger"
)

var ict = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Bangkok")
	if err != nil {
		panic("the tests need time zone data: " + err.Error())
	}
	return loc
}()

type trigRig struct {
	tt    *TriggerTools
	store *trigger.Store
	now   time.Time
	woke  int
}

func newTrigRig(t *testing.T, tweak func(*TriggerTools)) *trigRig {
	t.Helper()
	store, err := trigger.Open(filepath.Join(t.TempDir(), "triggers.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	r := &trigRig{store: store, now: time.Date(2026, 10, 9, 10, 0, 0, 0, ict)} // a Friday
	r.tt = &TriggerTools{Store: store, Location: ict, Now: func() time.Time { return r.now }, Wake: func() { r.woke++ },
		Windows: map[string]time.Duration{"messenger": 24 * time.Hour}}
	if tweak != nil {
		tweak(r.tt)
	}
	return r
}

func (r *trigRig) call(name string, cc CallContext, args string) (string, error) {
	for _, tl := range r.tt.Tools() {
		if tl.Name() == name {
			return tl.Execute(context.Background(), cc, []byte(args))
		}
	}
	panic("no tool " + name)
}

var ann = CallContext{Channel: "telegram", ChatID: "42", UserID: "5", UserName: "Ann"}

func TestCreateAReminderForALaterTime(t *testing.T) {
	r := newTrigRig(t, nil)
	out, err := r.call("trigger_create", ann, `{"text":"Call Peter about the quote","at":"2026-10-09T15:00:00+07:00","title":"call Peter"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"#1", "remind", "call Peter", "Fri 2026-10-09 15:00 +07", "once"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer lacks %q:\n%s", want, out)
		}
	}
	got, _ := r.store.Get(context.Background(), "telegram", "42", 1)
	if got.Mode != trigger.ModeRemind || got.OwnerID != "5" || got.OwnerName != "Ann" || got.IsGroup || got.Recurring() ||
		!got.Next.Equal(time.Date(2026, 10, 9, 15, 0, 0, 0, ict)) || got.Zone != "Asia/Bangkok" || got.Text != "Call Peter about the quote" {
		t.Errorf("%+v", got)
	}
	if r.woke != 1 {
		t.Error("the scheduler must be woken")
	}
	// a time without an offset is read in the bot's time zone, or the one given
	if _, err := r.call("trigger_create", ann, `{"text":"x","at":"2026-10-10T08:30"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.call("trigger_create", ann, `{"text":"y","at":"2026-10-10 08:30","timezone":"UTC"}`); err != nil {
		t.Fatal(err)
	}
	l, _ := r.store.List(context.Background(), "telegram", "42")
	if !l[1].Next.Equal(time.Date(2026, 10, 10, 8, 30, 0, 0, ict)) || !l[2].Next.Equal(time.Date(2026, 10, 10, 8, 30, 0, 0, time.UTC)) || l[2].Zone != "UTC" {
		t.Errorf("%v %v (%s)", l[1].Next, l[2].Next, l[2].Zone)
	}
}

func TestCreateARepeatingTaskAndShowTheNextRuns(t *testing.T) {
	r := newTrigRig(t, nil)
	out, err := r.call("trigger_create", ann, `{"mode":"task","text":"Find interesting news for me and summarise them","cron":"0 7 * * *","timezone":"Asia/Bangkok"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"task", `cron "0 7 * * *"`, "Asia/Bangkok", "Sat 2026-10-10 07:00", "then Sun 2026-10-11 07:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer lacks %q:\n%s", want, out)
		}
	}
	got, _ := r.store.Get(context.Background(), "telegram", "42", 1)
	if got.Cron != "0 7 * * *" || got.Mode != trigger.ModeTask || !got.Recurring() {
		t.Errorf("%+v", got)
	}
}

func TestCreateRefusesWhatCannotWork(t *testing.T) {
	r := newTrigRig(t, nil)
	for name, tc := range map[string]struct {
		cc   CallContext
		args string
		want string
	}{
		"in the past":          {ann, `{"text":"x","at":"2026-10-09T09:00:00+07:00"}`, "already passed"},
		"far away":             {ann, `{"text":"x","at":"2029-01-01T09:00:00+07:00"}`, "two years"},
		"both at and cron":     {ann, `{"text":"x","at":"2026-10-10T09:00:00+07:00","cron":"0 7 * * *"}`, "not both"},
		"no schedule":          {ann, `{"text":"x"}`, "say when"},
		"no text":              {ann, `{"at":"2026-10-10T09:00:00+07:00"}`, "text is empty"},
		"bad cron":             {ann, `{"text":"x","cron":"every day"}`, "five fields"},
		"bad time":             {ann, `{"text":"x","at":"tomorrow afternoon"}`, "cannot read the time"},
		"bad zone":             {ann, `{"text":"x","at":"2026-10-10T09:00","timezone":"Mars/Base"}`, "unknown time zone"},
		"bad mode":             {ann, `{"text":"x","mode":"shout","at":"2026-10-10T09:00"}`, "remind or task"},
		"a task every minute":  {ann, `{"mode":"task","text":"x","cron":"* * * * *"}`, "too often"},
		"a task every 5 min":   {ann, `{"mode":"task","text":"x","cron":"*/5 * * * *"}`, "too often"},
		"never":                {ann, `{"text":"x","cron":"0 0 31 2 *"}`, "never comes"},
		"no chat":              {CallContext{Channel: "telegram"}, `{"text":"x","at":"2026-10-10T09:00"}`, "no chat"},
		"messenger, repeating": {CallContext{Channel: "messenger", ChatID: "9", UserID: "9"}, `{"text":"x","cron":"0 7 * * *"}`, "within 24h0m0s"},
		"messenger, too late":  {CallContext{Channel: "messenger", ChatID: "9", UserID: "9"}, `{"text":"x","at":"2026-10-11T10:00:00+07:00"}`, "next 23h0m0s"},
		"a long text":          {ann, `{"text":"` + strings.Repeat("x", 2001) + `","at":"2026-10-10T09:00"}`, "too long"},
	} {
		if _, err := r.call("trigger_create", tc.cc, tc.args); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %q", name, err, tc.want)
		}
	}
	if l, _ := r.store.List(context.Background(), "telegram", "42"); len(l) != 0 {
		t.Errorf("a refused request left %d triggers behind", len(l))
	}
	// a reminder may repeat every minute... and one within a messenger window is fine
	if _, err := r.call("trigger_create", ann, `{"text":"x","cron":"* * * * *"}`); err != nil {
		t.Errorf("a repeating reminder every minute: %v", err)
	}
	if _, err := r.call("trigger_create", CallContext{Channel: "messenger", ChatID: "9", UserID: "9"}, `{"text":"x","at":"2026-10-09T20:00:00+07:00"}`); err != nil {
		t.Errorf("a messenger reminder within the window: %v", err)
	}
}

func TestLimitsAndSwitches(t *testing.T) {
	r := newTrigRig(t, func(tt *TriggerTools) {
		tt.MaxPerChat = 2
		tt.NoTasks = true
		tt.CanSend = func(ch string) bool { return ch == "telegram" }
	})
	for i := 0; i < 2; i++ {
		if _, err := r.call("trigger_create", ann, `{"text":"x","at":"2026-10-10T09:00"}`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.call("trigger_create", ann, `{"text":"x","at":"2026-10-10T09:00"}`); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("the third: %v", err)
	}
	// another chat has its own allowance; tasks and unsupported channels are refused
	other := CallContext{Channel: "telegram", ChatID: "43", UserID: "6", UserName: "Bob"}
	if _, err := r.call("trigger_create", other, `{"mode":"task","text":"x","at":"2026-10-10T09:00"}`); err == nil || !strings.Contains(err.Error(), "switched off") {
		t.Errorf("task: %v", err)
	}
	if _, err := r.call("trigger_create", CallContext{Channel: "web", ChatID: "v", UserID: "v"}, `{"text":"x","at":"2026-10-10T09:00"}`); err == nil || !strings.Contains(err.Error(), "cannot send") {
		t.Errorf("channel: %v", err)
	}
	if _, err := r.call("trigger_create", other, `{"text":"x","at":"2026-10-10T09:00"}`); err != nil {
		t.Errorf("another chat: %v", err)
	}
}

func TestListShowsTheChatsTriggersOnly(t *testing.T) {
	r := newTrigRig(t, nil)
	out, _ := r.call("trigger_list", ann, `{}`)
	if !strings.Contains(out, "Nothing is scheduled") {
		t.Errorf("%s", out)
	}
	_, _ = r.call("trigger_create", ann, `{"text":"Call Peter about the quote","at":"2026-10-09T15:00:00+07:00","title":"call Peter"}`)
	_, _ = r.call("trigger_create", ann, `{"mode":"task","text":"Find news","cron":"0 7 * * *"}`)
	other := CallContext{Channel: "telegram", ChatID: "43", UserID: "6", UserName: "Bob"}
	_, _ = r.call("trigger_create", other, `{"text":"Bob's secret","at":"2026-10-09T16:00:00+07:00"}`)
	out, err := r.call("trigger_list", ann, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"2 scheduled item(s)", "#1 [remind, active]", `"call Peter"`, "text: Call Peter about the quote", "#2 [task, active]", `cron "0 7 * * *"`, "set up by Ann"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Bob's secret") {
		t.Errorf("another chat's trigger is listed:\n%s", out)
	}
}

func TestUpdateChangesPausesAndResumes(t *testing.T) {
	r := newTrigRig(t, nil)
	_, _ = r.call("trigger_create", ann, `{"text":"Call Peter","at":"2026-10-09T15:00:00+07:00"}`)
	woke := r.woke
	out, err := r.call("trigger_update", ann, `{"id":1,"at":"2026-10-09T16:30:00+07:00","text":"Call Peter and Paul"}`)
	if err != nil || !strings.Contains(out, "Fri 2026-10-09 16:30") {
		t.Fatalf("%v\n%s", err, out)
	}
	got, _ := r.store.Get(context.Background(), "telegram", "42", 1)
	if got.Text != "Call Peter and Paul" || !got.Next.Equal(time.Date(2026, 10, 9, 16, 30, 0, 0, ict)) || r.woke == woke {
		t.Errorf("%+v", got)
	}
	// from once to repeating
	if _, err := r.call("trigger_update", ann, `{"id":1,"cron":"0 9 * * mon-fri"}`); err != nil {
		t.Fatal(err)
	}
	got, _ = r.store.Get(context.Background(), "telegram", "42", 1)
	if got.Cron != "0 9 * * mon-fri" || !got.At.IsZero() || !got.Next.Equal(time.Date(2026, 10, 12, 9, 0, 0, 0, ict)) {
		t.Errorf("%+v", got)
	}
	// pause, and resume
	if _, err := r.call("trigger_update", ann, `{"id":1,"enabled":false}`); err != nil {
		t.Fatal(err)
	}
	got, _ = r.store.Get(context.Background(), "telegram", "42", 1)
	if got.Status != trigger.StatusPaused {
		t.Errorf("%+v", got)
	}
	if d, _ := r.store.Due(context.Background(), r.now.Add(100*24*time.Hour), 10); len(d) != 0 {
		t.Error("a paused trigger is due")
	}
	r.now = r.now.Add(48 * time.Hour) // resumed later: the next time is worked out again
	if _, err := r.call("trigger_update", ann, `{"id":1,"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	got, _ = r.store.Get(context.Background(), "telegram", "42", 1)
	if got.Status != trigger.StatusActive || !got.Next.After(r.now) {
		t.Errorf("%+v", got)
	}
	// changing to a task makes the task's minimum interval apply
	if _, err := r.call("trigger_create", ann, `{"text":"Stretch","cron":"*/10 * * * *"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.call("trigger_update", ann, `{"id":2,"mode":"task"}`); err == nil || !strings.Contains(err.Error(), "too often") {
		t.Errorf("to a task: %v", err)
	}
	// a change to the time zone keeps the schedule
	if _, err := r.call("trigger_update", ann, `{"id":2,"timezone":"UTC"}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.store.Get(context.Background(), "telegram", "42", 2); got.Zone != "UTC" || got.Cron != "*/10 * * * *" {
		t.Errorf("%+v", got)
	}
}

func TestOnlyTheOwnerChangesOrDeletes(t *testing.T) {
	r := newTrigRig(t, nil)
	group := CallContext{Channel: "telegram", ChatID: "-100", UserID: "5", UserName: "Ann", IsGroup: true}
	bob := CallContext{Channel: "telegram", ChatID: "-100", UserID: "6", UserName: "Bob", IsGroup: true}
	if _, err := r.call("trigger_create", group, `{"text":"Team meeting","at":"2026-10-09T15:00:00+07:00"}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.store.Get(context.Background(), "telegram", "-100", 1); !got.IsGroup {
		t.Error("a trigger of a group must know it")
	}
	// everybody in the group sees it
	if out, _ := r.call("trigger_list", bob, `{}`); !strings.Contains(out, "Team meeting") {
		t.Errorf("%s", out)
	}
	// but only Ann changes it
	for _, call := range [][2]string{{"trigger_update", `{"id":1,"text":"hacked"}`}, {"trigger_delete", `{"id":1}`}} {
		if _, err := r.call(call[0], bob, call[1]); err == nil || !strings.Contains(err.Error(), "only Ann") {
			t.Errorf("%s by someone else: %v", call[0], err)
		}
	}
	if got, _ := r.store.Get(context.Background(), "telegram", "-100", 1); got.Text != "Team meeting" {
		t.Errorf("%+v", got)
	}
	// and nobody reaches the trigger of another chat
	if _, err := r.call("trigger_delete", ann, `{"id":1}`); err == nil || !strings.Contains(err.Error(), "no trigger #1") {
		t.Errorf("another chat: %v", err)
	}
	out, err := r.call("trigger_delete", group, `{"id":1}`)
	if err != nil || !strings.Contains(out, "Deleted trigger #1") {
		t.Fatalf("%v %s", err, out)
	}
	if l, _ := r.store.List(context.Background(), "telegram", "-100"); len(l) != 0 {
		t.Error("not deleted")
	}
	if _, err := r.call("trigger_update", group, `{"id":99}`); err == nil || !strings.Contains(err.Error(), "no trigger #99") {
		t.Errorf("%v", err)
	}
}

func TestAFiredOneTimeTriggerCanBeScheduledAgain(t *testing.T) {
	r := newTrigRig(t, nil)
	_, _ = r.call("trigger_create", ann, `{"text":"Call Peter","at":"2026-10-09T15:00:00+07:00"}`)
	got, _ := r.store.Get(context.Background(), "telegram", "42", 1)
	_ = r.store.Finish(context.Background(), got, "ok", "")
	if _, err := r.call("trigger_update", ann, `{"id":1,"text":"again"}`); err == nil || !strings.Contains(err.Error(), "already run") {
		t.Errorf("%v", err)
	}
	if _, err := r.call("trigger_update", ann, `{"id":1,"at":"2026-10-09T18:00:00+07:00"}`); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.store.Get(context.Background(), "telegram", "42", 1); got.Status != trigger.StatusActive {
		t.Errorf("%+v", got)
	}
}

func TestTriggerToolsHint(t *testing.T) {
	r := newTrigRig(t, nil)
	var found int
	for _, tl := range r.tt.Tools() {
		if h, ok := tl.(Hinter); ok {
			found++
			if !strings.Contains(h.Hint(), "0 7 * * *") || !strings.Contains(h.Hint(), "trigger_create") {
				t.Errorf("hint: %s", h.Hint())
			}
		}
	}
	if found != 1 {
		t.Errorf("%d tools add a hint, want exactly one", found)
	}
}
