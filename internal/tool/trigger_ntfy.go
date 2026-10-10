package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/authapon/jannyq/internal/ntfy"
	"github.com/authapon/jannyq/internal/trigger"
)

// deliveryArgs are the arguments of trigger_create and trigger_update that say
// how a scheduled message is delivered.
type deliveryArgs struct {
	Notify    *string `json:"notify"`
	NtfyTopic *string `json:"ntfy_topic"`
	Priority  *int    `json:"priority"`
}

func (d deliveryArgs) any() bool { return d.Notify != nil || d.NtfyTopic != nil || d.Priority != nil }

// ntfyParams is the part of the parameter schema for ntfy, with a trailing
// comma; empty when ntfy is not set up.
func (tt *TriggerTools) ntfyParams(create bool) string {
	if tt.Ntfy == nil {
		return ""
	}
	notify := `"How to deliver it: chat (in this chat), ntfy (a push notification to the user's phone only) or both. ` +
		`Default: the user's saved choice (ntfy_settings), else chat. Use ntfy or both only when the user asks for a notification on their phone, or has chosen that.`
	if !create {
		notify = `"Change how it is delivered: chat, ntfy or both.`
	}
	return `"notify":{"type":"string","enum":["chat","ntfy","both"],"description":` + notify + `"},` +
		`"ntfy_topic":{"type":"string","description":"The ntfy topic for this one only, when the user wants it on another topic than their saved one (letters, digits, - and _). Empty removes it. Not allowed in a group chat."},` +
		`"priority":{"type":"integer","minimum":0,"maximum":5,"description":"ntfy priority 1 (min) to 5 (urgent); 3 is normal; 0 = default. Only when the user asks for it."},`
}

// ntfyWindowHint is added to the errors about the 24 hour window.
func (tt *TriggerTools) ntfyWindowHint() string {
	if tt.Ntfy == nil {
		return ""
	}
	return "; or send it through ntfy only (notify=ntfy), which has no such limit"
}

func parseNotify(s string) (string, error) {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case trigger.NotifyChat, trigger.NotifyNtfy, trigger.NotifyBoth:
		return v, nil
	default:
		return "", fmt.Errorf("notify must be chat, ntfy or both, not %q", s)
	}
}

func involvesNtfy(n string) bool { return n == trigger.NotifyNtfy || n == trigger.NotifyBoth }

// applyDelivery sets how a trigger is delivered from the arguments, checking
// the rules: ntfy must be set up, a topic must exist, and nothing private is
// asked for in a group.
func (tt *TriggerTools) applyDelivery(ctx context.Context, cc CallContext, t *trigger.Trigger, d deliveryArgs, create bool) error {
	if t.Notify == "" {
		t.Notify = trigger.NotifyChat
	}
	if tt.Ntfy == nil {
		if d.any() {
			// the arguments are not offered; a model that sends them anyway is told
			if d.Notify != nil && strings.ToLower(strings.TrimSpace(*d.Notify)) != trigger.NotifyChat || d.NtfyTopic != nil && strings.TrimSpace(*d.NtfyTopic) != "" {
				return errors.New("ntfy is not set up on this bot, so messages can only be delivered in the chat")
			}
		}
		return nil
	}
	var prefs trigger.Prefs
	if tt.Store != nil {
		p, err := tt.Store.GetPrefs(ctx, cc.Channel, cc.UserID)
		if err != nil {
			return err
		}
		prefs = p
	}

	if d.NtfyTopic != nil {
		name := strings.TrimSpace(*d.NtfyTopic)
		if name != "" {
			if cc.IsGroup {
				return errors.New("a topic must not be given in a group chat: everybody there can read it. Ask the user to do it in a private chat with the bot")
			}
			if _, err := tt.Ntfy.Topic(name); err != nil {
				return err
			}
		}
		t.NtfyTopic = name
	}
	switch {
	case d.Notify != nil:
		n, err := parseNotify(*d.Notify)
		if err != nil {
			return err
		}
		t.Notify = n
	case create:
		t.Notify = trigger.NotifyChat
		if involvesNtfy(prefs.DefaultNotify) {
			t.Notify = prefs.DefaultNotify
		} else if t.NtfyTopic != "" {
			t.Notify = trigger.NotifyBoth // a topic was named: the user wants it there
		}
	}
	if d.Priority != nil {
		if *d.Priority < 0 || *d.Priority > 5 {
			return errors.New("priority must be from 1 (min) to 5 (urgent), or 0 for the default")
		}
		t.Priority = *d.Priority
	}
	if !involvesNtfy(t.Notify) || (!create && !d.any()) {
		return nil
	}
	if cc.IsGroup && t.Notify == trigger.NotifyNtfy {
		return errors.New("in a group chat the message must also go to the chat (notify=both), because the group would not know about it otherwise; " +
			"or ask the user to set it up in a private chat")
	}
	if t.NtfyTopic == "" && prefs.NtfyTopic == "" {
		return errors.New("there is no ntfy topic: ask the user for the topic they subscribe to in the ntfy app, then pass it as ntfy_topic " +
			"(for this reminder only) or save it with ntfy_settings (set) in a private chat")
	}
	return nil
}

// deliveryText says, for a list, how a trigger is delivered. The topic is shown
// only to the person who set it up, and not in a group.
func deliveryText(t trigger.Trigger, viewer CallContext) string {
	if !involvesNtfy(t.Notify) {
		return ""
	}
	var sb strings.Builder
	if t.Notify == trigger.NotifyNtfy {
		sb.WriteString("; delivered: ntfy only (the chat is not told)")
	} else {
		sb.WriteString("; delivered: in the chat and through ntfy")
	}
	switch {
	case t.NtfyTopic == "":
		sb.WriteString(", to the owner's saved topic")
	case viewer.UserID == t.OwnerID && !viewer.IsGroup:
		fmt.Fprintf(&sb, ", topic %q", t.NtfyTopic)
	default:
		sb.WriteString(", to a topic of its own")
	}
	if t.Priority != 0 {
		fmt.Fprintf(&sb, ", priority %d", t.Priority)
	}
	return sb.String()
}

// deliveryAdvice is added to the result of create and update.
func (tt *TriggerTools) deliveryAdvice(t trigger.Trigger, cc CallContext) string {
	if !involvesNtfy(t.Notify) {
		return ""
	}
	adv := "\nIt goes to ntfy: the user must be subscribed to the topic in the ntfy app to receive it."
	if t.Notify == trigger.NotifyBoth && tt.Windows[cc.Channel] > 0 {
		adv += fmt.Sprintf(" On %s the chat copy arrives only if the user has written within %s; the ntfy copy always does.", cc.Channel, tt.Windows[cc.Channel])
	}
	return adv
}

// --- ntfy_settings ---

type ntfySettings struct{ tt *TriggerTools }

func (ntfySettings) Name() string { return "ntfy_settings" }

func (ntfySettings) Description() string {
	return "Show, save, remove or test the user's ntfy topic (where their scheduled reminders can be pushed to their phone) " +
		"and whether new reminders go to ntfy by default. Saving is for a private chat only."
}

func (ntfySettings) Parameters() []byte {
	return []byte(`{"type":"object","properties":{` +
		`"action":{"type":"string","enum":["show","set","clear","test"],"description":"show (default), set (save topic and/or default), clear (forget the topic and the default), test (send a test notification to the saved topic)."},` +
		`"topic":{"type":"string","description":"For set: the ntfy topic the user subscribes to (letters, digits, - and _)."},` +
		`"default_notify":{"type":"string","enum":["chat","ntfy","both"],"description":"For set: where new reminders go unless the user says otherwise."}},` +
		`"required":[]}`)
}

func (ntfySettings) Hint() string {
	return "Phone notifications: reminders and tasks can also be pushed to the user's phone through ntfy. If the user wants that, ask for the ntfy topic they " +
		"subscribe to (or, if they have none, suggest a long, hard-to-guess name: on a public ntfy server anyone who knows a topic can read it), " +
		"save it with ntfy_settings (set; private chat only), then use trigger_create with notify=ntfy (phone only) or notify=both. " +
		"The user may choose per reminder: some by chat only, some through ntfy, and a different topic for one of them with ntfy_topic. " +
		"Never write a topic into a group chat."
}

func (n ntfySettings) Execute(ctx context.Context, cc CallContext, args []byte) (string, error) {
	var in struct {
		Action        string `json:"action"`
		Topic         string `json:"topic"`
		DefaultNotify string `json:"default_notify"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &in); err != nil {
			return "", fmt.Errorf("invalid arguments: %w", err)
		}
	}
	tt := n.tt
	if cc.Channel == "" || cc.UserID == "" {
		return "", errors.New("cannot tell who this is")
	}
	p, err := tt.Store.GetPrefs(ctx, cc.Channel, cc.UserID)
	if err != nil {
		return "", err
	}
	switch act := strings.ToLower(strings.TrimSpace(in.Action)); act {
	case "", "show":
		return n.show(cc, p), nil
	case "set":
		if cc.IsGroup {
			return "", errors.New("a topic is private and must not be written in a group chat: ask the user to do this in a private chat with the bot")
		}
		topic := strings.TrimSpace(in.Topic)
		if topic == "" && strings.TrimSpace(in.DefaultNotify) == "" {
			return "", errors.New("say what to save: topic and/or default_notify")
		}
		if topic != "" {
			if _, err := tt.Ntfy.Topic(topic); err != nil {
				return "", err
			}
			p.NtfyTopic = topic
		}
		if d := strings.TrimSpace(in.DefaultNotify); d != "" {
			v, err := parseNotify(d)
			if err != nil {
				return "", err
			}
			if v == trigger.NotifyChat {
				v = "" // chat is what it is without a choice
			}
			p.DefaultNotify = v
		}
		if involvesNtfy(p.DefaultNotify) && p.NtfyTopic == "" {
			return "", errors.New("give the topic too: reminders cannot go to ntfy by default without one")
		}
		if err := tt.Store.SetPrefs(ctx, p); err != nil {
			return "", err
		}
		if tt.Log != nil {
			tt.Log.Info("ntfy settings saved", "channel", cc.Channel, "user", cc.UserID, "topic", ntfy.Short(p.NtfyTopic), "default", p.DefaultNotify)
		}
		return "Saved. " + n.show(cc, p) + "\nTell the user to subscribe to this topic in the ntfy app, and offer to send a test notification (action test)." + publicWarning(tt), nil
	case "clear":
		if err := tt.Store.SetPrefs(ctx, trigger.Prefs{Channel: cc.Channel, UserID: cc.UserID}); err != nil {
			return "", err
		}
		return "The saved ntfy topic and default are removed. Reminders that used that topic will now be delivered in the chat instead, with a note.", nil
	case "test":
		topic := p.NtfyTopic
		if topic == "" {
			return "", errors.New("no ntfy topic is saved: ask the user for it and save it with action set (in a private chat)")
		}
		err := tt.Ntfy.Publish(ctx, ntfy.Message{Topic: topic, Title: "jannyq", Body: "Test: reminders can reach this phone. ✓", Tags: []string{"white_check_mark"}})
		if err != nil {
			return "", fmt.Errorf("the test notification failed: %w", err)
		}
		return "A test notification was sent to the saved topic. Ask the user whether it arrived on their phone.", nil
	default:
		return "", fmt.Errorf("action must be show, set, clear or test, not %q", in.Action)
	}
}

func (n ntfySettings) show(cc CallContext, p trigger.Prefs) string {
	var sb strings.Builder
	switch {
	case p.NtfyTopic == "":
		sb.WriteString("No ntfy topic is saved for this user.")
	case cc.IsGroup:
		sb.WriteString("An ntfy topic is saved for this user (not shown in a group chat).")
	default:
		fmt.Fprintf(&sb, "Saved ntfy topic: %q.", p.NtfyTopic)
	}
	switch p.DefaultNotify {
	case trigger.NotifyNtfy:
		sb.WriteString(" New reminders go to ntfy only unless the user says otherwise.")
	case trigger.NotifyBoth:
		sb.WriteString(" New reminders go to the chat and to ntfy unless the user says otherwise.")
	default:
		sb.WriteString(" New reminders go to the chat unless the user asks for ntfy.")
	}
	return sb.String()
}

// publicWarning reminds, on the public ntfy.sh server, that topics are open.
func publicWarning(tt *TriggerTools) string {
	if tt.Ntfy != nil && strings.Contains(tt.Ntfy.BaseURL, "ntfy.sh") {
		return "\nThis is the public ntfy.sh server: anyone who knows the topic name can read what is sent to it. " +
			"Say so, and advise a long name that cannot be guessed."
	}
	return ""
}
