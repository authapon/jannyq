package tool

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/authapon/jannyq/internal/ntfy"
	"github.com/authapon/jannyq/internal/trigger"
)

type fakeNtfy struct {
	mu   sync.Mutex
	got  []map[string]any
	code int
}

func (f *fakeNtfy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.code != 0 {
		w.WriteHeader(f.code)
		return
	}
	f.got = append(f.got, m)
	w.WriteHeader(200)
}

func ntfyRig(t *testing.T) (*trigRig, *fakeNtfy) {
	t.Helper()
	f := &fakeNtfy{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return newTrigRig(t, func(tt *TriggerTools) {
		tt.Ntfy = &ntfy.Client{BaseURL: srv.URL, TopicPrefix: "jq-"}
	}), f
}

func getTrigger(t *testing.T, r *trigRig, id int64) trigger.Trigger {
	t.Helper()
	got, err := r.store.Get(context.Background(), "telegram", "42", id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

const soon = `"at":"2026-10-09T15:00:00+07:00"`

func TestNtfyToolsExistOnlyWhenNtfyIsSetUp(t *testing.T) {
	off := newTrigRig(t, nil)
	for _, tl := range off.tt.Tools() {
		if tl.Name() == "ntfy_settings" || strings.Contains(string(tl.Parameters()), "ntfy") {
			t.Errorf("%s mentions ntfy although it is not set up", tl.Name())
		}
	}
	if _, err := off.call("trigger_create", ann, `{"text":"x",`+soon+`,"notify":"ntfy"}`); err == nil || !strings.Contains(err.Error(), "not set up") {
		t.Errorf("notify=ntfy without ntfy: %v", err)
	}
	if _, err := off.call("trigger_create", ann, `{"text":"x",`+soon+`,"notify":"chat"}`); err != nil {
		t.Errorf("notify=chat is harmless: %v", err)
	}

	on, _ := ntfyRig(t)
	names := map[string]bool{}
	for _, tl := range on.tt.Tools() {
		names[tl.Name()] = true
		var v map[string]any
		if err := json.Unmarshal(tl.Parameters(), &v); err != nil {
			t.Errorf("%s: parameters are not JSON: %v\n%s", tl.Name(), err, tl.Parameters())
		}
	}
	if !names["ntfy_settings"] {
		t.Error("ntfy_settings missing")
	}
	for _, name := range []string{"trigger_create", "trigger_update"} {
		for _, tl := range on.tt.Tools() {
			if tl.Name() == name {
				var v struct {
					Properties map[string]any `json:"properties"`
				}
				_ = json.Unmarshal(tl.Parameters(), &v)
				for _, k := range []string{"notify", "ntfy_topic", "priority"} {
					if v.Properties[k] == nil {
						t.Errorf("%s lacks %s", name, k)
					}
				}
			}
		}
	}
	hints := 0
	for _, tl := range on.tt.Tools() {
		if h, ok := tl.(Hinter); ok && strings.Contains(h.Hint(), "ntfy") {
			hints++
		}
	}
	if hints != 1 {
		t.Errorf("%d ntfy hints", hints)
	}
}

func TestSettingsSaveShowTestAndClear(t *testing.T) {
	r, f := ntfyRig(t)
	out, err := r.call("ntfy_settings", ann, `{}`)
	if err != nil || !strings.Contains(out, "No ntfy topic") {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := r.call("ntfy_settings", ann, `{"action":"test"}`); err == nil || !strings.Contains(err.Error(), "no ntfy topic") {
		t.Errorf("test without a topic: %v", err)
	}
	for name, args := range map[string]string{
		"bad topic":         `{"action":"set","topic":"my topic"}`,
		"nothing":           `{"action":"set"}`,
		"bad default":       `{"action":"set","topic":"a","default_notify":"sms"}`,
		"default w/o topic": `{"action":"set","default_notify":"both"}`,
		"bad action":        `{"action":"wipe"}`,
	} {
		if _, err := r.call("ntfy_settings", ann, args); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	out, err = r.call("ntfy_settings", ann, `{"action":"set","topic":"ann-phone-x7","default_notify":"both"}`)
	if err != nil || !strings.Contains(out, `"ann-phone-x7"`) || !strings.Contains(out, "chat and to ntfy") {
		t.Fatalf("%q %v", out, err)
	}
	p, _ := r.store.GetPrefs(context.Background(), "telegram", "5")
	if p.NtfyTopic != "ann-phone-x7" || p.DefaultNotify != trigger.NotifyBoth {
		t.Fatalf("%+v", p)
	}
	// another user is not affected
	if p, _ := r.store.GetPrefs(context.Background(), "telegram", "6"); p.NtfyTopic != "" {
		t.Fatalf("%+v", p)
	}
	if _, err := r.call("ntfy_settings", ann, `{"action":"test"}`); err != nil {
		t.Fatal(err)
	}
	if len(f.got) != 1 || f.got[0]["topic"] != "jq-ann-phone-x7" {
		t.Fatalf("test notification: %v", f.got)
	}
	// only the default can be changed later
	if _, err := r.call("ntfy_settings", ann, `{"action":"set","default_notify":"chat"}`); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.store.GetPrefs(context.Background(), "telegram", "5"); p.NtfyTopic != "ann-phone-x7" || p.DefaultNotify != "" {
		t.Fatalf("%+v", p)
	}
	if _, err := r.call("ntfy_settings", ann, `{"action":"clear"}`); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.store.GetPrefs(context.Background(), "telegram", "5"); p.NtfyTopic != "" || p.DefaultNotify != "" {
		t.Fatalf("%+v", p)
	}
	f.code = 403
	r.call("ntfy_settings", ann, `{"action":"set","topic":"abc"}`)
	if _, err := r.call("ntfy_settings", ann, `{"action":"test"}`); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("a refused test must say so: %v", err)
	}
}

func TestSettingsAreNotShownOrChangedInAGroup(t *testing.T) {
	r, _ := ntfyRig(t)
	if _, err := r.call("ntfy_settings", ann, `{"action":"set","topic":"ann-secret-topic"}`); err != nil {
		t.Fatal(err)
	}
	group := ann
	group.IsGroup = true
	if _, err := r.call("ntfy_settings", group, `{"action":"set","topic":"other-topic"}`); err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("set in a group: %v", err)
	}
	out, err := r.call("ntfy_settings", group, `{"action":"show"}`)
	if err != nil || strings.Contains(out, "ann-secret-topic") || !strings.Contains(out, "saved") {
		t.Errorf("show in a group must not reveal the topic: %q %v", out, err)
	}
	if p, _ := r.store.GetPrefs(context.Background(), "telegram", "5"); p.NtfyTopic != "ann-secret-topic" {
		t.Errorf("%+v", p)
	}
}

func TestCreateWithNotifyModesTopicsAndDefaults(t *testing.T) {
	r, _ := ntfyRig(t)
	ctx := context.Background()

	// no topic anywhere
	if _, err := r.call("trigger_create", ann, `{"text":"x",`+soon+`,"notify":"ntfy"}`); err == nil || !strings.Contains(err.Error(), "no ntfy topic") {
		t.Errorf("no topic: %v", err)
	}
	if l, _ := r.store.List(ctx, "telegram", "42"); len(l) != 0 {
		t.Fatal("a refused request left a trigger behind")
	}
	// a topic for this one only; without notify it means "also there"
	out, err := r.call("trigger_create", ann, `{"text":"x",`+soon+`,"ntfy_topic":"work-alerts","priority":4}`)
	if err != nil {
		t.Fatal(err)
	}
	if g := getTrigger(t, r, 1); g.Notify != trigger.NotifyBoth || g.NtfyTopic != "work-alerts" || g.Priority != 4 {
		t.Errorf("%+v", g)
	}
	for _, want := range []string{"ntfy", `"work-alerts"`, "priority 4", "subscribed"} {
		if !strings.Contains(out, want) {
			t.Errorf("answer lacks %q:\n%s", want, out)
		}
	}
	// save a topic and a default: new ones follow it, and can still say otherwise
	if _, err := r.call("ntfy_settings", ann, `{"action":"set","topic":"ann-phone","default_notify":"ntfy"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.call("trigger_create", ann, `{"text":"y",`+soon+`}`); err != nil {
		t.Fatal(err)
	}
	if g := getTrigger(t, r, 2); g.Notify != trigger.NotifyNtfy || g.NtfyTopic != "" {
		t.Errorf("default not applied: %+v", g)
	}
	if _, err := r.call("trigger_create", ann, `{"text":"z",`+soon+`,"notify":"chat"}`); err != nil {
		t.Fatal(err)
	}
	if g := getTrigger(t, r, 3); g.Notify != trigger.NotifyChat {
		t.Errorf("%+v", g)
	}
	for name, args := range map[string]string{
		"bad notify":   `{"text":"x",` + soon + `,"notify":"sms"}`,
		"bad topic":    `{"text":"x",` + soon + `,"notify":"ntfy","ntfy_topic":"a b"}`,
		"bad priority": `{"text":"x",` + soon + `,"priority":9}`,
	} {
		if _, err := r.call("trigger_create", ann, args); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	// the list shows the topic to its owner, in a private chat, and nowhere else
	out, _ = r.call("trigger_list", ann, `{}`)
	if !strings.Contains(out, `"work-alerts"`) || !strings.Contains(out, "saved topic") {
		t.Errorf("owner's list:\n%s", out)
	}
	bob := CallContext{Channel: "telegram", ChatID: "42", UserID: "6", UserName: "Bob"}
	out, _ = r.call("trigger_list", bob, `{}`)
	if strings.Contains(out, "work-alerts") || !strings.Contains(out, "topic of its own") {
		t.Errorf("another user's list must hide the topic:\n%s", out)
	}
	grp := ann
	grp.IsGroup = true
	out, _ = r.call("trigger_list", grp, `{}`)
	if strings.Contains(out, "work-alerts") {
		t.Errorf("a group's list must hide the topic:\n%s", out)
	}
}

func TestGroupRules(t *testing.T) {
	r, _ := ntfyRig(t)
	if _, err := r.call("ntfy_settings", ann, `{"action":"set","topic":"ann-phone"}`); err != nil {
		t.Fatal(err)
	}
	grp := CallContext{Channel: "telegram", ChatID: "42", UserID: "5", UserName: "Ann", IsGroup: true}
	if _, err := r.call("trigger_create", grp, `{"text":"x",`+soon+`,"notify":"both","ntfy_topic":"leak-me"}`); err == nil || !strings.Contains(err.Error(), "group") {
		t.Errorf("a topic in a group: %v", err)
	}
	if _, err := r.call("trigger_create", grp, `{"text":"x",`+soon+`,"notify":"ntfy"}`); err == nil || !strings.Contains(err.Error(), "notify=both") {
		t.Errorf("ntfy only in a group: %v", err)
	}
	// both, using the topic the user saved in private, is fine
	if _, err := r.call("trigger_create", grp, `{"text":"x",`+soon+`,"notify":"both"}`); err != nil {
		t.Errorf("both in a group: %v", err)
	}
}

func TestUpdateChangesDeliveryAndKeepsItOtherwise(t *testing.T) {
	r, _ := ntfyRig(t)
	r.call("ntfy_settings", ann, `{"action":"set","topic":"ann-phone"}`)
	if _, err := r.call("trigger_create", ann, `{"text":"x",`+soon+`,"notify":"both","priority":5}`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.call("trigger_update", ann, `{"id":1,"title":"renamed"}`); err != nil {
		t.Fatal(err)
	}
	if g := getTrigger(t, r, 1); g.Notify != trigger.NotifyBoth || g.Priority != 5 || g.Title != "renamed" {
		t.Errorf("an unrelated change altered the delivery: %+v", g)
	}
	if _, err := r.call("trigger_update", ann, `{"id":1,"notify":"ntfy","ntfy_topic":"other-topic","priority":0}`); err != nil {
		t.Fatal(err)
	}
	if g := getTrigger(t, r, 1); g.Notify != trigger.NotifyNtfy || g.NtfyTopic != "other-topic" || g.Priority != 0 {
		t.Errorf("%+v", g)
	}
	if _, err := r.call("trigger_update", ann, `{"id":1,"ntfy_topic":""}`); err != nil {
		t.Fatal(err) // falls back to the saved topic
	}
	if g := getTrigger(t, r, 1); g.NtfyTopic != "" || g.Notify != trigger.NotifyNtfy {
		t.Errorf("%+v", g)
	}
	// without any topic, ntfy cannot be asked for
	r.call("ntfy_settings", ann, `{"action":"clear"}`)
	if _, err := r.call("trigger_update", ann, `{"id":1,"priority":2}`); err == nil || !strings.Contains(err.Error(), "no ntfy topic") {
		t.Errorf("%v", err)
	}
	if _, err := r.call("trigger_update", ann, `{"id":1,"notify":"chat"}`); err != nil {
		t.Errorf("back to the chat: %v", err)
	}
	// but an update that does not touch the delivery is not blocked by a topic that went away
	r.call("trigger_update", ann, `{"id":1,"notify":"ntfy","ntfy_topic":"t1"}`)
	r.call("ntfy_settings", ann, `{"action":"clear"}`)
	if _, err := r.call("trigger_update", ann, `{"id":1,"enabled":false}`); err != nil {
		t.Errorf("pausing: %v", err)
	}
	// only the owner may change it
	bob := CallContext{Channel: "telegram", ChatID: "42", UserID: "6", UserName: "Bob"}
	if _, err := r.call("trigger_update", bob, `{"id":1,"notify":"chat"}`); err == nil {
		t.Error("another user changed the delivery")
	}
}

func TestMessengerWindowDoesNotLimitNtfyOnly(t *testing.T) {
	r, _ := ntfyRig(t)
	m := CallContext{Channel: "messenger", ChatID: "9", UserID: "9", UserName: "Mia"}
	r.call("ntfy_settings", m, `{"action":"set","topic":"mia-phone"}`)
	later := `"at":"2026-10-20T10:00:00+07:00"`
	if _, err := r.call("trigger_create", m, `{"text":"x",`+later+`}`); err == nil || !strings.Contains(err.Error(), "ntfy only") {
		t.Errorf("the window must apply to the chat, and the error point to ntfy: %v", err)
	}
	if _, err := r.call("trigger_create", m, `{"text":"x",`+later+`,"notify":"ntfy"}`); err != nil {
		t.Errorf("ntfy only, far ahead: %v", err)
	}
	if _, err := r.call("trigger_create", m, `{"text":"x","cron":"0 7 * * *","notify":"ntfy"}`); err != nil {
		t.Errorf("ntfy only, repeating: %v", err)
	}
	out, err := r.call("trigger_create", m, `{"text":"x","cron":"0 8 * * *","notify":"both"}`)
	if err == nil {
		t.Errorf("a repeating chat copy on messenger must still be refused: %s", out)
	}
	// switching a far-off ntfy-only reminder back to the chat is refused by the window
	if _, err := r.call("trigger_update", m, `{"id":1,"notify":"chat"}`); err == nil || !strings.Contains(err.Error(), "within") {
		t.Errorf("%v", err)
	}
	if g, _ := r.store.Get(context.Background(), "messenger", "9", 1); g.Notify != trigger.NotifyNtfy {
		t.Errorf("a refused update changed the trigger: %+v", g)
	}
}
