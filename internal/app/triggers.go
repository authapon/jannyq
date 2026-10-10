package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/authapon/jannyq/internal/channel"
	"github.com/authapon/jannyq/internal/config"
	"github.com/authapon/jannyq/internal/metrics"
	"github.com/authapon/jannyq/internal/ntfy"
	"github.com/authapon/jannyq/internal/router"
	"github.com/authapon/jannyq/internal/session"
	"github.com/authapon/jannyq/internal/tool"
	"github.com/authapon/jannyq/internal/trigger"
)

// triggerSystem is the scheduling of reminders and tasks: their store, the
// tools the model manages them with, and the scheduler that fires them.
type triggerSystem struct {
	store     *trigger.Store
	sched     *trigger.Scheduler
	tools     *tool.TriggerTools
	notifiers map[string]channel.Notifier
	windows   map[string]time.Duration
	ntfy      *ntfy.Client // nil without --ntfy-url
}

// newTriggers opens the trigger database; it returns nil when --triggers is off.
func newTriggers(cfg *config.Config, loc *time.Location, inst metrics.Instruments, log *slog.Logger) (*triggerSystem, error) {
	if !cfg.Triggers {
		return nil, nil
	}
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	store, err := trigger.Open(filepath.Join(cfg.DataDir, "triggers.db"))
	if err != nil {
		return nil, fmt.Errorf("open the trigger database: %w", err)
	}
	ts := &triggerSystem{store: store, notifiers: map[string]channel.Notifier{}, windows: map[string]time.Duration{}}
	ts.sched = &trigger.Scheduler{
		Store: store, Grace: cfg.TriggerGrace, Log: log,
		OnRun: func(mode, result string) { inst.TriggerRuns.Inc(mode, result) },
	}
	if cfg.NtfyURL != "" {
		ts.ntfy = &ntfy.Client{BaseURL: cfg.NtfyURL, Token: cfg.NtfyToken, TopicPrefix: cfg.NtfyTopicPrefix, Log: log}
		log.Info("ntfy on for scheduled messages", "server", cfg.NtfyURL, "prefix", cfg.NtfyTopicPrefix, "token", cfg.NtfyToken != "")
	}
	ts.tools = &tool.TriggerTools{
		Store: store, Location: loc, Windows: ts.windows,
		CanSend:    func(ch string) bool { _, ok := ts.notifiers[ch]; return ok },
		MaxPerChat: cfg.TriggerMaxPerChat, MinTaskInterval: cfg.TriggerMinInterval, NoTasks: !cfg.TriggerTasks,
		Ntfy: ts.ntfy, Wake: ts.sched.Wake, Log: log,
	}
	return ts, nil
}

// register adds the trigger tools.
func (ts *triggerSystem) register(reg *tool.Registry) {
	for _, t := range ts.tools.Tools() {
		reg.Register(t)
	}
}

// attach connects the scheduler to the router and the channels that can send on their own.
func (ts *triggerSystem) attach(rt *router.Router, channels []channel.Channel, log *slog.Logger) {
	for _, ch := range channels {
		n, ok := ch.(channel.Notifier)
		if !ok {
			continue
		}
		ts.notifiers[ch.Name()] = n
		if w, ok := ch.(channel.Windowed); ok {
			ts.windows[ch.Name()] = w.Window()
		}
	}
	rt.SetNotifiers(ts.notifiers)
	rt.SetNtfy(ts.ntfy, ts.store.GetPrefs)
	ts.sched.Runner = trigger.RunnerFunc(rt.RunTrigger)
	log.Info("scheduled reminders and tasks on", "channels", len(ts.notifiers))
}

// forgetChats deletes the triggers of chats that no longer exist.
func (ts *triggerSystem) forgetChats(sessions *session.Manager, log *slog.Logger) {
	n, err := ts.store.DeleteOrphans(context.Background(), sessions.Exists)
	if err != nil {
		log.Warn("could not delete the triggers of deleted chats", "err", err)
		return
	}
	if n > 0 {
		log.Info("triggers of deleted chats removed", "count", n)
	}
}

// registerTriggerGauges exports how many triggers are active.
func registerTriggerGauges(reg *metrics.Registry, ts *triggerSystem) {
	if ts == nil {
		return
	}
	reg.GaugeFunc("jannyq_triggers_active", "Scheduled reminders and tasks that are active, by mode.", []string{"mode"}, func() []metrics.Sample {
		remind, task, err := ts.store.Stats(context.Background())
		if err != nil {
			return nil
		}
		return []metrics.Sample{{Labels: []string{trigger.ModeRemind}, Value: float64(remind)}, {Labels: []string{trigger.ModeTask}, Value: float64(task)}}
	})
}
