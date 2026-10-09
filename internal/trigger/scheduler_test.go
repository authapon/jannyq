package trigger

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeRunner struct {
	mu    sync.Mutex
	runs  []Trigger
	lates []time.Duration
	fn    func(call int, t Trigger) error
}

func (f *fakeRunner) Run(_ context.Context, t Trigger, late time.Duration) error {
	f.mu.Lock()
	f.runs = append(f.runs, t)
	f.lates = append(f.lates, late)
	call := len(f.runs)
	f.mu.Unlock()
	if f.fn != nil {
		return f.fn(call, t)
	}
	return nil
}

func (f *fakeRunner) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.runs) }

type rig struct {
	s     *Scheduler
	store *Store
	run   *fakeRunner
	now   time.Time
	mu    sync.Mutex
	outs  []string
}

func newScheduler(t *testing.T) *rig {
	t.Helper()
	store, _ := openTest(t)
	r := &rig{store: store, run: &fakeRunner{}, now: time.Unix(1_800_000_000, 0)}
	store.now = func() time.Time { return r.now }
	r.s = &Scheduler{Store: store, Runner: r.run, Grace: time.Hour, Now: func() time.Time { return r.now },
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnRun: func(mode, result string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.outs = append(r.outs, mode+":"+result)
		}}
	r.s.init()
	return r
}

// fire runs everything that is due and waits for the runs to end.
func (r *rig) fire() {
	var wg sync.WaitGroup
	r.s.fireDue(context.Background(), &wg, make(chan struct{}, 4))
	wg.Wait()
}

func (r *rig) get(id int64) Trigger {
	t, _ := r.store.Get(bg, "telegram", "42", id)
	return t
}

func TestSchedulerFiresAOneTimeTriggerOnce(t *testing.T) {
	r := newScheduler(t)
	id, _ := r.store.Create(bg, sample(r.now.Add(-30*time.Second)))
	r.fire()
	r.fire() // a second look must not run it again
	if r.run.count() != 1 || r.run.lates[0] != 30*time.Second {
		t.Fatalf("%d runs, lates %v", r.run.count(), r.run.lates)
	}
	if got := r.get(id); got.Status != StatusDone || got.LastResult != "ok" || got.Runs != 1 {
		t.Errorf("%+v", got)
	}
	if len(r.outs) != 1 || r.outs[0] != "remind:ok" {
		t.Errorf("outcomes %v", r.outs)
	}
}

func TestSchedulerLeavesFutureTriggersAlone(t *testing.T) {
	r := newScheduler(t)
	_, _ = r.store.Create(bg, sample(r.now.Add(time.Minute)))
	r.fire()
	if r.run.count() != 0 {
		t.Fatal("fired too early")
	}
	r.now = r.now.Add(61 * time.Second)
	r.fire()
	if r.run.count() != 1 {
		t.Fatal("did not fire when due")
	}
}

func TestARecurringTriggerMovesOnAndKeepsFiring(t *testing.T) {
	r := newScheduler(t)
	c := sample(r.now)
	c.Cron, c.At, c.Zone = "*/30 * * * *", time.Time{}, "UTC"
	cr, _ := ParseCron(c.Cron)
	c.Next, _ = cr.Next(r.now.Add(-time.Hour), time.UTC)
	c.Next = r.now.Add(-time.Minute)
	id, _ := r.store.Create(bg, c)
	r.fire()
	if r.run.count() != 1 {
		t.Fatalf("%d runs", r.run.count())
	}
	got := r.get(id)
	if got.Status != StatusActive || !got.Next.After(r.now) || got.Next.Sub(r.now) > 30*time.Minute {
		t.Errorf("next = %v (now %v)", got.Next, r.now)
	}
	first := got.Next
	r.now = first.Add(time.Second)
	r.fire()
	if r.run.count() != 2 || !r.get(id).Next.After(first) {
		t.Errorf("%d runs, next %v", r.run.count(), r.get(id).Next)
	}
}

func TestMissedOccurrencesWhileTheBotWasOff(t *testing.T) {
	r := newScheduler(t)
	// a recurring one, more than the grace period late: skipped, and moved on
	c := sample(r.now)
	c.Cron, c.At, c.Zone, c.Next = "0 7 * * *", time.Time{}, "UTC", r.now.Add(-5*time.Hour)
	rec, _ := r.store.Create(bg, c)
	// a one-time one, as late: still delivered, told how late
	once, _ := r.store.Create(bg, sample(r.now.Add(-5*time.Hour)))
	// a one-time one, a little late: delivered
	soon, _ := r.store.Create(bg, sample(r.now.Add(-10*time.Minute)))
	r.fire()
	if r.run.count() != 2 {
		t.Fatalf("%d runs, want the two one-time triggers", r.run.count())
	}
	if got := r.get(rec); got.LastResult != "skipped" || got.Runs != 0 || !got.Next.After(r.now) || got.Status != StatusActive {
		t.Errorf("recurring: %+v", got)
	}
	if r.get(once).Status != StatusDone || r.get(soon).Status != StatusDone {
		t.Error("one-time triggers must be delivered however late")
	}
	late := map[time.Duration]bool{}
	for _, l := range r.run.lates {
		late[l] = true
	}
	if !late[5*time.Hour] || !late[10*time.Minute] {
		t.Errorf("lates %v", r.run.lates)
	}
}

func TestARecurringTriggerWithinTheGraceIsRun(t *testing.T) {
	r := newScheduler(t)
	c := sample(r.now)
	c.Cron, c.At, c.Zone, c.Next = "0 7 * * *", time.Time{}, "UTC", r.now.Add(-20*time.Minute)
	_, _ = r.store.Create(bg, c)
	r.fire()
	if r.run.count() != 1 {
		t.Errorf("%d runs", r.run.count())
	}
}

func TestSchedulerRetriesWhileTheChatIsBusy(t *testing.T) {
	r := newScheduler(t)
	r.run.fn = func(call int, _ Trigger) error {
		if call < 3 {
			return &RetryError{After: time.Millisecond}
		}
		return nil
	}
	id, _ := r.store.Create(bg, sample(r.now.Add(-time.Second)))
	r.fire()
	if r.run.count() != 3 || r.get(id).LastResult != "ok" {
		t.Errorf("%d runs, %+v", r.run.count(), r.get(id))
	}
	// it gives up after a few tries and records a failure
	r2 := newScheduler(t)
	r2.run.fn = func(int, Trigger) error { return &RetryError{After: time.Millisecond} }
	id2, _ := r2.store.Create(bg, sample(r2.now.Add(-time.Second)))
	r2.fire()
	if r2.run.count() != maxRetries || r2.get(id2).LastResult != "failed" {
		t.Errorf("%d runs, %+v", r2.run.count(), r2.get(id2))
	}
}

func TestSchedulerRecordsFailuresAndDisables(t *testing.T) {
	r := newScheduler(t)
	r.run.fn = func(_ int, tr Trigger) error {
		if tr.Title == "disable me" {
			return &DisableError{Reason: "the owner is not allowed any more"}
		}
		return errors.New("the model is down")
	}
	failing, _ := r.store.Create(bg, sample(r.now.Add(-time.Second)))
	dis := sample(r.now.Add(-time.Second))
	dis.Title = "disable me"
	disabled, _ := r.store.Create(bg, dis)
	r.fire()
	if got := r.get(failing); got.LastResult != "failed" || got.LastError != "the model is down" || got.Fails != 1 || got.Status != StatusDone {
		t.Errorf("failing: %+v", got)
	}
	if got := r.get(disabled); got.Status != StatusDisabled || got.LastError != "the owner is not allowed any more" {
		t.Errorf("disabled: %+v", got)
	}
}

func TestSchedulerDisablesAnUnreadableSchedule(t *testing.T) {
	r := newScheduler(t)
	c := sample(r.now.Add(-time.Second))
	c.Cron = "not a schedule"
	id, _ := r.store.Create(bg, c)
	r.fire()
	if got := r.get(id); got.Status != StatusDisabled || r.run.count() != 0 {
		t.Errorf("%+v, %d runs", got, r.run.count())
	}
}

func TestSchedulerRunWakesForNewTriggers(t *testing.T) {
	store, _ := openTest(t)
	run := &fakeRunner{}
	s := &Scheduler{Store: store, Runner: run, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	// the scheduler is idle (sleeping up to a minute): a new trigger and a wake-up must be enough
	time.Sleep(50 * time.Millisecond)
	_, _ = store.Create(bg, sample(time.Now().Add(-time.Second)))
	s.Wake()
	deadline := time.Now().Add(3 * time.Second)
	for run.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if run.count() != 1 {
		t.Fatal("a trigger created while the scheduler sleeps was not fired")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run does not stop")
	}
}
