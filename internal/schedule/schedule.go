// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package schedule turns a service's ballast.schedule label into concrete
// firing times, and runs a fleet of jobs against those times with bounded
// concurrency.
//
// Two schedule families exist per the label grammar's Fork 6. Raw 5-field
// cron expressions and "@every <dur>" are literal: they fire exactly when
// robfig/cron says they do, no adjustment. The four period aliases,
// @hourly, @daily, @weekly, and @monthly, are splayed instead of firing at
// their canonical boundary: each job gets a deterministic per-name offset
// so a fleet of services all set to (say) @daily does not all wake up and
// hit the disk or uplink in the same instant.
//
// The splay offset for @daily/@weekly/@monthly lands somewhere inside a
// configured local-time window (default 01:00-05:00); for @hourly it lands
// somewhere inside the hour. Either way the offset is derived from a stable
// hash of the job name, so a given service always gets the same slot,
// including across restarts, without any state needing to be persisted.
package schedule

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"
)

// NextFunc computes the next firing time strictly after "after". Every
// implementation here guarantees a result strictly later than "after", so
// repeated calls (each fed the previous result) never stall or loop.
type NextFunc func(after time.Time) time.Time

// ErrEmptySchedule is returned by Parse when given a blank expression.
var ErrEmptySchedule = errors.New("schedule: expression is empty")

// Window is the local time-of-day range that @daily/@weekly/@monthly slots
// are splayed across. Start and End are offsets from local midnight. End
// may be numerically less than or equal to Start to express a window that
// wraps past midnight (e.g. 22:00 to 02:00); Duration handles that.
type Window struct {
	Start time.Duration
	End   time.Duration
}

// ParseWindow parses two "HH:MM" clock times into a Window.
func ParseWindow(start, end string) (Window, error) {
	s, err := parseClockTime(start)
	if err != nil {
		return Window{}, err
	}
	e, err := parseClockTime(end)
	if err != nil {
		return Window{}, err
	}
	if s == e {
		return Window{}, fmt.Errorf("schedule: window start %q and end %q must differ", start, end)
	}
	return Window{Start: s, End: e}, nil
}

// Duration is the length of the window, handling wraparound past midnight.
func (w Window) Duration() time.Duration {
	d := w.End - w.Start
	if d <= 0 {
		d += 24 * time.Hour
	}
	return d
}

// parseClockTime parses an "HH:MM" string into a duration since local
// midnight.
func parseClockTime(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("schedule: invalid window time %q: %w", s, err)
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// Parse turns a ballast.schedule expression into a NextFunc. name seeds the
// deterministic splay applied to the four period aliases; window bounds
// where @daily/@weekly/@monthly land. Raw 5-field cron and "@every <dur>"
// are always parsed literally via robfig/cron and never splayed, regardless
// of splay.
//
// splay is config.Config.Splay (or schedule.Config.Splay), resolved to a
// concrete bool by the caller: true (the default) splays the four period
// aliases as described above; false parses them the same way parseCron
// would parse any other expression, which for these particular strings
// means robfig/cron's own descriptor support (its Parser includes the
// Descriptor option), landing each alias on its canonical, unsplayed
// boundary (top of the hour, midnight, Sunday midnight, the 1st at
// midnight) instead.
func Parse(name, expr string, window Window, splay bool) (NextFunc, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, ErrEmptySchedule
	}

	if splay {
		switch expr {
		case "@hourly":
			return nextHourly(name), nil
		case "@daily":
			return nextDaily(name, window), nil
		case "@weekly":
			return nextWeekly(name, window), nil
		case "@monthly":
			return nextMonthly(name, window), nil
		}
	}

	return parseCron(expr)
}

// splayOffset derives a deterministic offset in [0, mod) from name, using
// fnv-1a so the value is stable across process restarts and Go versions.
func splayOffset(name string, mod int64) int64 {
	if mod <= 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64() % uint64(mod))
}

// nextHourly splays across the hour: the firing minute is
// hash(name) mod 60, stable per job.
func nextHourly(name string) NextFunc {
	minute := int(splayOffset(name, 60))
	return func(after time.Time) time.Time {
		loc := after.Location()
		candidate := time.Date(after.Year(), after.Month(), after.Day(), after.Hour(), minute, 0, 0, loc)
		if !candidate.After(after) {
			candidate = candidate.Add(time.Hour)
		}
		return candidate
	}
}

// slotOffset is the splayed time-of-day offset from local midnight for a
// @daily/@weekly/@monthly job: the window start plus hash(name) mod the
// window's duration.
func slotOffset(name string, window Window) time.Duration {
	off := time.Duration(splayOffset(name, int64(window.Duration()/time.Second))) * time.Second
	return window.Start + off
}

// nextDaily fires once per day at the job's splayed slot.
func nextDaily(name string, window Window) NextFunc {
	slot := slotOffset(name, window)
	return func(after time.Time) time.Time {
		loc := after.Location()
		day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)
		candidate := day.Add(slot)
		if !candidate.After(after) {
			candidate = candidate.Add(24 * time.Hour)
		}
		return candidate
	}
}

// nextWeekly fires once per week, anchored to Monday, at the job's splayed
// slot. The canonical boundary (which weekday) is not splayed, only the
// time of day within it.
func nextWeekly(name string, window Window) NextFunc {
	slot := slotOffset(name, window)
	return func(after time.Time) time.Time {
		loc := after.Location()
		day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, loc)
		daysSinceMonday := (int(day.Weekday()) + 6) % 7
		weekStart := day.AddDate(0, 0, -daysSinceMonday)
		candidate := weekStart.Add(slot)
		if !candidate.After(after) {
			candidate = candidate.AddDate(0, 0, 7)
		}
		return candidate
	}
}

// nextMonthly fires once per month, anchored to the 1st, at the job's
// splayed slot. The canonical boundary (the 1st) is not splayed, only the
// time of day within it.
func nextMonthly(name string, window Window) NextFunc {
	slot := slotOffset(name, window)
	return func(after time.Time) time.Time {
		loc := after.Location()
		monthStart := time.Date(after.Year(), after.Month(), 1, 0, 0, 0, 0, loc)
		candidate := monthStart.Add(slot)
		if !candidate.After(after) {
			candidate = monthStart.AddDate(0, 1, 0).Add(slot)
		}
		return candidate
	}
}
