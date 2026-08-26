// SPDX-License-Identifier: GPL-3.0-or-later

package schedule

import (
	"testing"
	"time"
)

func mustWindow(t *testing.T, start, end string) Window {
	t.Helper()
	w, err := ParseWindow(start, end)
	if err != nil {
		t.Fatalf("ParseWindow(%q, %q): %v", start, end, err)
	}
	return w
}

func TestParseEmptyExpression(t *testing.T) {
	if _, err := Parse("svc", "", mustWindow(t, "01:00", "05:00")); err != ErrEmptySchedule {
		t.Fatalf("got err %v, want ErrEmptySchedule", err)
	}
	if _, err := Parse("svc", "   ", mustWindow(t, "01:00", "05:00")); err != ErrEmptySchedule {
		t.Fatalf("got err %v, want ErrEmptySchedule", err)
	}
}

func TestParseInvalidExpression(t *testing.T) {
	_, err := Parse("svc", "not a schedule", mustWindow(t, "01:00", "05:00"))
	if err == nil {
		t.Fatal("expected an error for an unparseable expression")
	}
}

func TestParseCronLiteral(t *testing.T) {
	next, err := Parse("svc", "0 3 * * *", mustWindow(t, "01:00", "05:00"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	after := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	got := next(after)
	want := time.Date(2026, 8, 26, 3, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestParseEveryLiteral(t *testing.T) {
	next, err := Parse("svc", "@every 6h", mustWindow(t, "01:00", "05:00"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	after := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	got := next(after)
	want := after.Add(6 * time.Hour)
	if !got.Equal(want) {
		t.Fatalf("next = %v, want %v", got, want)
	}
}

func TestSplayIsStableAndDeterministic(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	after := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	next1, err := Parse("photo-lab", "@daily", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	next2, err := Parse("photo-lab", "@daily", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	got1 := next1(after)
	got2 := next2(after)
	if !got1.Equal(got2) {
		t.Fatalf("same job name produced different slots: %v vs %v", got1, got2)
	}
}

func TestHourlySplayWithinHourAndDistinctPerName(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	after := time.Date(2026, 8, 25, 10, 30, 0, 0, time.UTC)

	nextA, err := Parse("service-a", "@hourly", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	nextB, err := Parse("service-b", "@hourly", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	gotA := nextA(after)
	gotB := nextB(after)

	if gotA.Equal(gotB) {
		t.Fatalf("expected distinct hourly slots for different job names, both got %v", gotA)
	}
	for _, got := range []time.Time{gotA, gotB} {
		if got.Second() != 0 || got.Nanosecond() != 0 {
			t.Fatalf("hourly fire must land on a whole minute, got %v", got)
		}
		if !got.After(after) {
			t.Fatalf("next fire %v must be after %v", got, after)
		}
		if got.Sub(after) > time.Hour {
			t.Fatalf("hourly fire %v is more than an hour after %v", got, after)
		}
	}
}

func TestDailySplayLandsInsideWindow(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	after := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	next, err := Parse("firefly-db", "@daily", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := next(after)

	dayStart := time.Date(got.Year(), got.Month(), got.Day(), 0, 0, 0, 0, got.Location())
	offset := got.Sub(dayStart)
	if offset < window.Start || offset >= window.Start+window.Duration() {
		t.Fatalf("daily slot %v (offset %v) not inside window [%v, %v)", got, offset, window.Start, window.Start+window.Duration())
	}
}

// TestDailySplayDistinctAcrossThreeServices proves the fnv splay spreads a
// small fleet of services all set to the same period alias across
// different slots inside the window, not just two (the existing
// TestHourlySplayWithinHourAndDistinctPerName only proves pairwise
// distinctness for @hourly). This is the unit-level half of the itest
// suite's multi-service splay proof (test/integration/run-splay.sh), which
// demonstrates concurrency serialization with real @every firings instead
// of a real @daily wait, and leans on this test for the splay-slot
// distinctness claim.
func TestDailySplayDistinctAcrossThreeServices(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	after := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	names := []string{"ballast-itest-splay-a", "ballast-itest-splay-b", "ballast-itest-splay-c"}
	slots := make(map[string]time.Time, len(names))
	for _, name := range names {
		next, err := Parse(name, "@daily", window)
		if err != nil {
			t.Fatalf("Parse(%q): %v", name, err)
		}
		slots[name] = next(after)
	}

	for i, a := range names {
		for _, b := range names[i+1:] {
			if slots[a].Equal(slots[b]) {
				t.Fatalf("services %q and %q landed on the same @daily slot %v, want distinct slots", a, b, slots[a])
			}
		}
	}
}

func TestDailyDoesNotFireAtCanonicalMidnight(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	after := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)

	next, err := Parse("svc", "@daily", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := next(after)
	if got.Hour() == 0 && got.Minute() == 0 {
		t.Fatalf("expected a splayed slot, got canonical midnight %v", got)
	}
}

func TestDailyAdvancesToNextDayOncePassed(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	next, err := Parse("svc", "@daily", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	first := next(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC))
	second := next(first)

	if !second.After(first) {
		t.Fatalf("second fire %v must be after first %v", second, first)
	}
	if second.Sub(first) != 24*time.Hour {
		t.Fatalf("expected consecutive daily fires 24h apart, got %v apart", second.Sub(first))
	}
}

func TestWeeklyAnchoredToMonday(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	next, err := Parse("svc", "@weekly", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := next(time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)) // a Tuesday
	if got.Weekday() != time.Monday {
		t.Fatalf("expected weekly fire on Monday, got %v (%v)", got, got.Weekday())
	}
}

func TestMonthlyAnchoredToFirst(t *testing.T) {
	window := mustWindow(t, "01:00", "05:00")
	next, err := Parse("svc", "@monthly", window)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := next(time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC))
	if got.Day() != 1 {
		t.Fatalf("expected monthly fire on the 1st, got day %d (%v)", got.Day(), got)
	}
	if got.Month() != time.September {
		t.Fatalf("expected next monthly fire in September, got %v", got.Month())
	}
}

func TestParseWindowWraparound(t *testing.T) {
	w, err := ParseWindow("22:00", "02:00")
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	if w.Duration() != 4*time.Hour {
		t.Fatalf("wraparound window duration = %v, want 4h", w.Duration())
	}
}

func TestParseWindowRejectsEqualBounds(t *testing.T) {
	if _, err := ParseWindow("01:00", "01:00"); err == nil {
		t.Fatal("expected an error when window start equals end")
	}
}
