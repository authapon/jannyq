package trigger

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Runner carries out a trigger that has come due.
type Runner interface {
	// Run does what the trigger says. late is how long after its time it is.
	// It may return a *RetryError (the chat is busy), a *DisableError (the
	// trigger may not run any more) or any other error (the run failed).
	Run(ctx context.Context, t Trigger, late time.Duration) error
}

// RunnerFunc makes a function a Runner.
type RunnerFunc func(ctx context.Context, t Trigger, late time.Duration) error

// Run calls f.
func (f RunnerFunc) Run(ctx context.Context, t Trigger, late time.Duration) error {
	return f(ctx, t, late)
}

// RetryError asks the scheduler to try the same run again after a while.
type RetryError struct{ After time.Duration }

func (e *RetryError) Error() string { return fmt.Sprintf("try again in %s", e.After) }

// DisableError tells the scheduler to switch the trigger off for good.
type DisableError struct{ Reason string }

func (e *DisableError) Error() string { return "trigger disabled: " + e.Reason }

// Scheduler fires triggers when they come due.
type Scheduler struct {
	Store  *Store
	Runner Runner
	// Grace is how late a trigger may be run. A recurring trigger that is later
	// (the bot was off) skips that occurrence; a one-time trigger is always run,
	// and is told how late it is.
	Grace time.Duration
	// MaxConcurrent bounds the triggers being run at once (default 4).
	MaxConcurrent int
	// OnRun, if set, is called with the mode and the result ("ok", "failed",
	// "skipped") of every occurrence.
	OnRun func(mode, result string)
	Log   *slog.Logger
	Now   func() time.Time

	wake chan struct{}
}

const (
	maxRetries = 5
	idleCheck  = time.Minute // the longest the scheduler sleeps, in case the clock jumps
)

func (s *Scheduler) init() {
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = 4
	}
	if s.Grace <= 0 {
		s.Grace = time.Hour
	}
	if s.wake == nil {
		s.wake = make(chan struct{}, 1)
	}
}

// Wake makes the scheduler look at the store again, after a trigger was added
// or changed. Call it after Init or Run has started.
func (s *Scheduler) Wake() {
	s.init()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run fires triggers until ctx ends; it waits for the runs in progress.
func (s *Scheduler) Run(ctx context.Context) {
	s.init()
	sem := make(chan struct{}, s.MaxConcurrent)
	var wg sync.WaitGroup
	defer wg.Wait()
	var lastPrune time.Time
	for {
		s.fireDue(ctx, &wg, sem)
		if now := s.Now(); now.Sub(lastPrune) > time.Hour {
			lastPrune = now
			if n, err := s.Store.Prune(ctx, now.Add(-7*24*time.Hour)); err != nil {
				s.Log.Warn("could not delete old triggers", "err", err)
			} else if n > 0 {
				s.Log.Info("finished one-time triggers deleted", "count", n)
			}
		}
		wait := idleCheck
		if next, ok, err := s.Store.NextDue(ctx); err == nil && ok {
			if d := next.Sub(s.Now()); d < wait {
				wait = max(d, 0)
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-s.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// fireDue starts a run for every trigger that is due.
func (s *Scheduler) fireDue(ctx context.Context, wg *sync.WaitGroup, sem chan struct{}) {
	for round := 0; round < 20 && ctx.Err() == nil; round++ { // bounded: a row that cannot be claimed must not spin
		now := s.Now()
		due, err := s.Store.Due(ctx, now, 50)
		if err != nil {
			s.Log.Error("could not read the due triggers", "err", err)
			return
		}
		for _, t := range due {
			s.take(ctx, t, now, wg, sem)
		}
		if len(due) < 50 {
			return
		}
	}
}

// take claims one due trigger and runs it (or skips a late occurrence). It
// reports whether the trigger was dealt with.
func (s *Scheduler) take(ctx context.Context, t Trigger, now time.Time, wg *sync.WaitGroup, sem chan struct{}) bool {
	late := now.Sub(t.Next)
	var next time.Time
	if t.Recurring() {
		c, err := ParseCron(t.Cron)
		if err != nil {
			s.Log.Error("a stored schedule cannot be read: trigger disabled", "id", t.ID, "err", err)
			_ = s.Store.Disable(ctx, t.ID, "the schedule could not be read: "+err.Error())
			return true
		}
		next, _ = c.Next(now, t.Location()) // zero when it never comes again
	}
	claimed, err := s.Store.Claim(ctx, t, next)
	if err != nil {
		s.Log.Error("could not claim a trigger", "id", t.ID, "err", err)
		return false
	}
	if !claimed {
		return true // changed or taken by somebody else in the meantime
	}
	if t.Recurring() && late > s.Grace {
		s.Log.Info("trigger skipped: it came due while the bot was not running", "id", t.ID, "mode", t.Mode,
			"channel", t.Channel, "chat", t.ChatID, "late", late.Round(time.Second))
		_ = s.Store.Note(ctx, t.ID, "skipped", fmt.Sprintf("due %s, the bot was not running", t.Next.Format(time.RFC3339)))
		s.report(t.Mode, "skipped")
		return true
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return
		}
		s.run(ctx, t, late)
	}()
	return true
}

func (s *Scheduler) report(mode, result string) {
	if s.OnRun != nil {
		s.OnRun(mode, result)
	}
}

// run carries out one occurrence, retrying while the chat is busy.
func (s *Scheduler) run(ctx context.Context, t Trigger, late time.Duration) {
	start := s.Now()
	var err error
	for attempt := 0; attempt < maxRetries; attempt++ {
		err = s.Runner.Run(ctx, t, late)
		var retry *RetryError
		if !errors.As(err, &retry) {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(retry.After):
		}
		late = s.Now().Sub(t.Next)
	}
	if ctx.Err() != nil {
		return // stopping: a one-time trigger is taken up again at the next start
	}
	result, errText := "ok", ""
	var dis *DisableError
	switch {
	case errors.As(err, &dis):
		_ = s.Store.Disable(ctx, t.ID, dis.Reason)
		result, errText = "disabled", dis.Reason
	case err != nil:
		result, errText = "failed", err.Error()
		_ = s.Store.Finish(ctx, t, result, errText)
	default:
		_ = s.Store.Finish(ctx, t, result, "")
	}
	attrs := []any{"id", t.ID, "mode", t.Mode, "channel", t.Channel, "chat", t.ChatID, "owner", t.OwnerID,
		"late", late.Round(time.Second), "result", result, "took", s.Now().Sub(start).Round(time.Millisecond)}
	if errText != "" {
		attrs = append(attrs, "err", errText)
	}
	if result == "ok" {
		s.Log.Info("trigger fired", attrs...)
	} else {
		s.Log.Warn("trigger not delivered", attrs...)
	}
	s.report(t.Mode, result)
}
