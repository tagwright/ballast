// SPDX-License-Identifier: GPL-3.0-or-later

package schedule

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// everyFunc builds a NextFunc that behaves like "@every d", for tests that
// need precise control over an entry's schedule without going through
// Parse.
func everyFunc(d time.Duration) NextFunc {
	return func(after time.Time) time.Time { return after.Add(d) }
}

func namesOf(entries []*entry) []string {
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.job.Name
	}
	return names
}

func TestNewAppliesDefaultWindowAndConcurrency(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	want := mustWindow(t, defaultWindowStart, defaultWindowEnd)
	if s.window != want {
		t.Fatalf("window = %+v, want %+v", s.window, want)
	}
	if s.concurrency != 1 {
		t.Fatalf("concurrency = %d, want 1", s.concurrency)
	}
}

// TestNewDefaultsSplayOnWhenNil proves the Scheduler-level half of the
// BALLAST_SPLAY fix: a Config that leaves Splay nil (the zero value for a
// *bool, and what a caller who never touches the field produces) must still
// splay by default, not silently disable it.
func TestNewDefaultsSplayOnWhenNil(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.splay {
		t.Fatal("Scheduler.splay = false with a nil Config.Splay, want true (splay defaults on)")
	}
}

// TestNewHonorsExplicitSplayFalse proves an explicit Splay=false actually
// reaches the Scheduler, the other half of the fix: before this pass,
// Config.Splay was parsed and stored but Scheduler never read it at all.
func TestNewHonorsExplicitSplayFalse(t *testing.T) {
	disabled := false
	s, err := New(Config{Splay: &disabled})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if s.splay {
		t.Fatal("Scheduler.splay = true with Config.Splay = false, want false")
	}

	if err := s.Add(Job{Name: "svc", Schedule: "@daily"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.mu.Lock()
	next := s.entries["svc"].next
	s.mu.Unlock()

	got := next(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC))
	if got.Hour() != 0 || got.Minute() != 0 {
		t.Fatalf("splay disabled via Scheduler: expected @daily to fire at canonical midnight, got %v", got)
	}
}

func TestNewRejectsInvalidWindow(t *testing.T) {
	if _, err := New(Config{WindowStart: "not-a-time"}); err == nil {
		t.Fatal("expected an error for an invalid window")
	}
}

func TestAddAndRemove(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.Add(Job{Name: "svc", Schedule: "@daily"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	s.mu.Lock()
	_, ok := s.entries["svc"]
	s.mu.Unlock()
	if !ok {
		t.Fatal("expected job to be registered")
	}

	s.Remove("svc")
	s.mu.Lock()
	_, ok = s.entries["svc"]
	s.mu.Unlock()
	if ok {
		t.Fatal("expected job to be removed")
	}

	// Removing an absent job must not panic.
	s.Remove("does-not-exist")
}

func TestAddReplacesExistingByName(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Add(Job{Name: "svc", Schedule: "@daily"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(Job{Name: "svc", Schedule: "@every 5m"}); err != nil {
		t.Fatalf("Add (replace): %v", err)
	}
	s.mu.Lock()
	got := s.entries["svc"].job.Schedule
	s.mu.Unlock()
	if got != "@every 5m" {
		t.Fatalf("schedule after replace = %q, want %q", got, "@every 5m")
	}
}

func TestAddInvalidScheduleIsNotRegistered(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Add(Job{Name: "svc", Schedule: "garbage"}); err == nil {
		t.Fatal("expected an error for an invalid schedule")
	}
	s.mu.Lock()
	_, ok := s.entries["svc"]
	s.mu.Unlock()
	if ok {
		t.Fatal("job with an invalid schedule must not be registered")
	}
}

func TestCollectDueDispatchesReadyAndReportsSoonest(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	s.entries["ready"] = &entry{
		job:      Job{Name: "ready"},
		next:     everyFunc(time.Minute),
		nextFire: now.Add(-time.Second),
	}
	s.entries["future"] = &entry{
		job:      Job{Name: "future"},
		next:     everyFunc(time.Minute),
		nextFire: now.Add(30 * time.Second),
	}

	soonest, ready := s.collectDue()

	if names := namesOf(ready); len(names) != 1 || names[0] != "ready" {
		t.Fatalf("ready = %v, want [ready]", names)
	}
	if !ready[0].running {
		t.Fatal("dispatched entry should be marked running")
	}
	if !soonest.Equal(now.Add(30 * time.Second)) {
		t.Fatalf("soonest = %v, want %v", soonest, now.Add(30*time.Second))
	}
}

func TestCollectDueSkipsOverlappingRun(t *testing.T) {
	s, err := New(Config{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })

	var buf bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&buf, nil))

	// Overdue by more than one interval, and still marked running: the
	// overlap rule must skip every missed firing (not queue a backlog) and
	// land nextFire strictly past now.
	s.entries["slow"] = &entry{
		job:      Job{Name: "slow"},
		next:     everyFunc(time.Minute),
		nextFire: now.Add(-90 * time.Second),
		running:  true,
	}

	soonest, ready := s.collectDue()

	if len(ready) != 0 {
		t.Fatalf("expected no dispatch while the previous run is in flight, got %v", namesOf(ready))
	}
	if !soonest.After(now) {
		t.Fatalf("soonest %v should have advanced strictly past now %v", soonest, now)
	}
	if !strings.Contains(buf.String(), "skipping overlapping run") {
		t.Fatalf("expected an overlap-skip log entry, got: %s", buf.String())
	}
}

func TestRunFiresRegisteredJob(t *testing.T) {
	s, err := New(Config{Concurrency: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	calls := 0
	fired := make(chan struct{}, 1)

	err = s.Add(Job{
		Name:     "tick",
		Schedule: "@every 10ms",
		Run: func(ctx context.Context) {
			mu.Lock()
			calls++
			mu.Unlock()
			select {
			case fired <- struct{}{}:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("job never fired")
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if calls == 0 {
		t.Fatal("expected at least one call")
	}
}

func TestRunSkipsOverlappingFire(t *testing.T) {
	s, err := New(Config{Concurrency: 1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	calls := 0
	started := make(chan struct{}, 1)
	release := make(chan struct{})

	err = s.Add(Job{
		Name:     "blocker",
		Schedule: "@every 20ms",
		Run: func(ctx context.Context) {
			mu.Lock()
			calls++
			mu.Unlock()
			select {
			case started <- struct{}{}:
			default:
			}
			<-release
		},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("job never started")
	}

	// Several scheduled ticks elapse while the first run is still blocked
	// on release. None of them should dispatch a second concurrent call
	// for the same job.
	time.Sleep(150 * time.Millisecond)

	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("calls = %d while the first run is still in flight, want 1 (overlapping fires must be skipped)", got)
	}

	close(release)
	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunHonorsConcurrencyAcrossDifferentJobs(t *testing.T) {
	s, err := New(Config{Concurrency: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	var mu sync.Mutex
	inFlight := 0
	maxInFlight := 0
	release := make(chan struct{})

	run := func(ctx context.Context) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		<-release
		mu.Lock()
		inFlight--
		mu.Unlock()
	}

	if err := s.Add(Job{Name: "a", Schedule: "@every 10ms", Run: run}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := s.Add(Job{Name: "b", Schedule: "@every 10ms", Run: run}); err != nil {
		t.Fatalf("Add: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(runDone)
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		reached := maxInFlight >= 2
		mu.Unlock()
		if reached {
			break
		}
		select {
		case <-deadline:
			t.Fatal("never observed two jobs running concurrently")
		case <-time.After(5 * time.Millisecond):
		}
	}

	close(release)
	cancel()

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
