// SPDX-License-Identifier: GPL-3.0-or-later

package schedule

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"
)

// Default splay window, per the label grammar's Fork 6, applied when a
// Config leaves WindowStart/WindowEnd empty.
const (
	defaultWindowStart = "01:00"
	defaultWindowEnd   = "05:00"
)

// idleWait is how long Run sleeps when no job is registered. Add wakes it
// immediately, so this only bounds the worst case for an empty Scheduler.
const idleWait = time.Hour

// Job is one scheduled unit of work: a named backup run on a schedule
// expression.
type Job struct {
	// Name identifies the job and seeds the deterministic splay for period
	// alias schedules. It must be stable across restarts (the service name,
	// per the grammar's convention) or the job's splay slot moves.
	Name string

	// Schedule is a 5-field cron expression, "@every <dur>", or one of the
	// splayed period aliases: @hourly, @daily, @weekly, @monthly.
	Schedule string

	// Run performs the job's work. It receives the Scheduler's Run context,
	// cancelled when the scheduler is shutting down.
	Run func(ctx context.Context)
}

// Config configures a Scheduler.
type Config struct {
	// WindowStart and WindowEnd bound the splay window for @daily/@weekly/
	// @monthly, as local "HH:MM" clock times. Both default to "01:00" and
	// "05:00" when left empty.
	WindowStart, WindowEnd string

	// Concurrency caps how many jobs run at once. Defaults to 1 (serial),
	// matching the grammar's disk/uplink-protection intent.
	Concurrency int
}

// entry is a registered job plus its scheduling state.
type entry struct {
	job      Job
	next     NextFunc
	nextFire time.Time
	running  bool
}

// Scheduler fires each registered Job on its own schedule and dispatches
// due jobs through a bounded worker pool. If a job's previous run is still
// in flight when it comes due again, that firing is skipped and logged
// rather than queued, so a slow backup never piles up a backlog.
//
// A Scheduler is safe for concurrent Add/Remove while Run is executing.
type Scheduler struct {
	window      Window
	concurrency int

	mu      sync.Mutex
	entries map[string]*entry
	now     func() time.Time

	wake chan struct{}
	sem  chan struct{}
	wg   sync.WaitGroup

	log *slog.Logger
}

// New builds a Scheduler from cfg, parsing its splay window once up front.
func New(cfg Config) (*Scheduler, error) {
	start := cfg.WindowStart
	if start == "" {
		start = defaultWindowStart
	}
	end := cfg.WindowEnd
	if end == "" {
		end = defaultWindowEnd
	}
	window, err := ParseWindow(start, end)
	if err != nil {
		return nil, err
	}

	concurrency := cfg.Concurrency
	if concurrency <= 0 {
		concurrency = 1
	}

	return &Scheduler{
		window:      window,
		concurrency: concurrency,
		entries:     make(map[string]*entry),
		now:         time.Now,
		wake:        make(chan struct{}, 1),
		sem:         make(chan struct{}, concurrency),
		log:         slog.Default(),
	}, nil
}

// SetClock overrides the Scheduler's notion of "now". Intended for tests,
// so splay and next-fire logic can be exercised without waiting on the real
// clock; production callers can leave the default time.Now in place. Call
// it before Run, or concurrently with it (it is mutex-protected like
// Add/Remove), keeping in mind Run only reads the clock at points it would
// otherwise call time.Now.
func (s *Scheduler) SetClock(now func() time.Time) {
	s.mu.Lock()
	s.now = now
	s.mu.Unlock()
}

// Add registers job, parsing its schedule and computing its first firing
// time. A job whose name already exists is replaced: its prior entry is
// discarded and a fresh next-fire is computed from the current time.
func (s *Scheduler) Add(job Job) error {
	next, err := Parse(job.Name, job.Schedule, s.window)
	if err != nil {
		return fmt.Errorf("schedule: add %q: %w", job.Name, err)
	}

	s.mu.Lock()
	s.entries[job.Name] = &entry{job: job, next: next}
	s.mu.Unlock()

	s.notifyWake()
	return nil
}

// Remove unregisters the named job. It is a no-op if the name is not
// registered. A run already in flight for that job is not interrupted; it
// simply will not be rescheduled.
func (s *Scheduler) Remove(name string) {
	s.mu.Lock()
	delete(s.entries, name)
	s.mu.Unlock()
}

// notifyWake interrupts a sleeping Run loop so it re-evaluates schedules
// immediately, e.g. after Add registers a job that is due sooner than
// whatever Run was already sleeping toward.
func (s *Scheduler) notifyWake() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run is the scheduler loop: it computes the soonest next firing across all
// registered jobs, sleeps until then (or until Add/Remove or ctx wakes it
// early), dispatches every job due at that instant, and repeats. Dispatch
// honors Concurrency via a bounded worker pool. Run returns once ctx is
// cancelled, after waiting for any in-flight jobs to finish.
func (s *Scheduler) Run(ctx context.Context) {
	defer s.wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		soonest, ready := s.collectDue()
		for _, e := range ready {
			s.dispatch(ctx, e)
		}
		if len(ready) > 0 {
			continue
		}

		wait := idleWait
		if !soonest.IsZero() {
			if d := soonest.Sub(s.clock()); d > 0 {
				wait = d
			} else {
				wait = 0
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// clock returns the Scheduler's current notion of "now".
func (s *Scheduler) clock() time.Time {
	s.mu.Lock()
	now := s.now
	s.mu.Unlock()
	return now()
}

// collectDue advances every entry's next-fire time up to the current
// instant, returning the entries ready to run right now and the soonest
// remaining next-fire time among the rest (zero if none are registered).
//
// An entry whose slot comes due while its previous run is still marked
// running is skipped (logged) rather than dispatched or queued: this is
// the overlap rule that keeps a slow job from piling up a backlog,
// including when the scheduler has fallen behind by more than one period.
func (s *Scheduler) collectDue() (soonest time.Time, ready []*entry) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()

	for _, e := range s.entries {
		if e.nextFire.IsZero() {
			e.nextFire = e.next(now)
		}

		for !e.nextFire.After(now) {
			if e.running {
				s.log.Warn("schedule: skipping overlapping run",
					"job", e.job.Name, "scheduled", e.nextFire)
				e.nextFire = e.next(e.nextFire)
				continue
			}
			e.running = true
			ready = append(ready, e)
			e.nextFire = e.next(e.nextFire)
			break
		}

		if soonest.IsZero() || e.nextFire.Before(soonest) {
			soonest = e.nextFire
		}
	}

	sort.Slice(ready, func(i, j int) bool { return ready[i].job.Name < ready[j].job.Name })
	return soonest, ready
}

// dispatch runs e's job in a worker goroutine, bounded by the Concurrency
// semaphore. e.running is already true (set by collectDue); dispatch clears
// it when the job finishes, or immediately if ctx is cancelled before a
// worker slot is acquired.
func (s *Scheduler) dispatch(ctx context.Context, e *entry) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			s.markDone(e)
			return
		}
		defer func() { <-s.sem }()

		defer s.markDone(e)
		e.job.Run(ctx)
	}()
}

// markDone clears e's running flag so a future firing is not skipped as an
// overlap.
func (s *Scheduler) markDone(e *entry) {
	s.mu.Lock()
	e.running = false
	s.mu.Unlock()
}
