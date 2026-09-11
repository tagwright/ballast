// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tagwright/ballast/internal/engine"
)

// ParseRetentionPolicy is Level-1 pure arithmetic that decides which backups
// survive a forget pass (task #620). A wrong parse is silent data loss on the
// day a restore matters, so these test the parse by invariant over the input
// space, not a single happy example: every recognized key, the string keys
// (within, keep-tags), and every malformed-input path failing closed rather than
// producing a wrong-but-plausible policy.

// TestParseRetentionAllIntegerKeys proves each recognized integer key lands in
// its own RetentionPolicy field, and only that field.
func TestParseRetentionAllIntegerKeys(t *testing.T) {
	got, err := ParseRetentionPolicy("last=1,hourly=2,daily=3,weekly=4,monthly=5,yearly=6")
	if err != nil {
		t.Fatalf("ParseRetentionPolicy: %v", err)
	}
	want := engine.RetentionPolicy{Last: 1, Hourly: 2, Daily: 3, Weekly: 4, Monthly: 5, Yearly: 6}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("policy = %+v, want %+v", got, want)
	}
}

// TestParseRetentionWithinAndKeepTags proves the two string-valued keys are
// carried through raw: within as a restic duration string, keep-tags accumulated
// into the slice.
func TestParseRetentionWithinAndKeepTags(t *testing.T) {
	got, err := ParseRetentionPolicy("within=7d,keep-tags=critical")
	if err != nil {
		t.Fatalf("ParseRetentionPolicy: %v", err)
	}
	if got.Within != "7d" {
		t.Errorf("Within = %q, want %q", got.Within, "7d")
	}
	if len(got.KeepTags) != 1 || got.KeepTags[0] != "critical" {
		t.Errorf("KeepTags = %v, want [critical]", got.KeepTags)
	}
}

// TestParseRetentionKeepTagsAccumulate proves repeated keep-tags terms append
// rather than overwrite, so multiple protected tags all survive.
func TestParseRetentionKeepTagsAccumulate(t *testing.T) {
	got, err := ParseRetentionPolicy("keep-tags=a,keep-tags=b")
	if err != nil {
		t.Fatalf("ParseRetentionPolicy: %v", err)
	}
	if len(got.KeepTags) != 2 || got.KeepTags[0] != "a" || got.KeepTags[1] != "b" {
		t.Errorf("KeepTags = %v, want [a b]", got.KeepTags)
	}
}

// TestParseRetentionDuplicateIntegerKeyLastWins pins the documented behavior for
// a repeated integer key: the later term overwrites the earlier one. This is an
// observed property worth locking, not an accident: a policy string that names
// daily twice resolves to the last value deterministically.
func TestParseRetentionDuplicateIntegerKeyLastWins(t *testing.T) {
	got, err := ParseRetentionPolicy("daily=7,daily=3")
	if err != nil {
		t.Fatalf("ParseRetentionPolicy: %v", err)
	}
	if got.Daily != 3 {
		t.Errorf("Daily = %d, want 3 (last term wins)", got.Daily)
	}
}

// TestParseRetentionNormalizesKeysAndTrims proves keys are lowercased and terms
// are trimmed, so whitespace and case in a hand-written policy string do not
// silently drop a dimension.
func TestParseRetentionNormalizesKeysAndTrims(t *testing.T) {
	got, err := ParseRetentionPolicy("  DAILY = 7 , Weekly=4 ")
	if err != nil {
		t.Fatalf("ParseRetentionPolicy: %v", err)
	}
	if got.Daily != 7 {
		t.Errorf("Daily = %d, want 7 (key should be case-insensitive, value trimmed)", got.Daily)
	}
	if got.Weekly != 4 {
		t.Errorf("Weekly = %d, want 4", got.Weekly)
	}
}

// TestParseRetentionSkipsBlankTerms proves empty terms (from trailing or doubled
// commas) are skipped rather than treated as malformed.
func TestParseRetentionSkipsBlankTerms(t *testing.T) {
	got, err := ParseRetentionPolicy("daily=7,,weekly=4,")
	if err != nil {
		t.Fatalf("ParseRetentionPolicy: %v", err)
	}
	if got.Daily != 7 || got.Weekly != 4 {
		t.Errorf("policy = %+v, want Daily=7 Weekly=4", got)
	}
}

// TestParseRetentionEmptyString proves a fully blank spec parses to the zero
// policy with no error (the caller layers its own default over the zero value).
func TestParseRetentionEmptyString(t *testing.T) {
	got, err := ParseRetentionPolicy("   ")
	if err != nil {
		t.Fatalf("ParseRetentionPolicy: %v", err)
	}
	if !isZeroRetention(got) {
		t.Errorf("blank spec = %+v, want the zero policy", got)
	}
}

// TestParseRetentionRejectsMalformed proves every malformed-input path fails
// closed with an error naming the offending term, rather than producing a
// wrong-but-plausible policy. A bad value must never be silently coerced to zero,
// which would delete every backup in that dimension.
func TestParseRetentionRejectsMalformed(t *testing.T) {
	tests := []struct {
		name    string
		spec    string
		wantSub string // substring the error should mention
	}{
		{"missing equals", "daily", "daily"},
		{"non-integer value", "daily=lots", "lots"},
		{"negative-looking non-int", "weekly=4.5", "4.5"},
		{"empty integer value", "daily=", "daily"},
		{"unknown key", "fortnightly=2", "fortnightly"},
		{"unknown key among valid", "daily=7,bogus=1", "bogus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseRetentionPolicy(tc.spec)
			if err == nil {
				t.Fatalf("ParseRetentionPolicy(%q) accepted malformed input", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

// TestDefaultRetentionPolicyFallback proves the Config-facing wrapper falls back
// to the sane global default when handed a blank spec, and passes a real spec
// straight through.
func TestDefaultRetentionPolicyFallback(t *testing.T) {
	got, err := defaultRetentionPolicy("")
	if err != nil {
		t.Fatalf("defaultRetentionPolicy(\"\"): %v", err)
	}
	want, err := ParseRetentionPolicy(defaultRetentionSpec)
	if err != nil {
		t.Fatalf("parsing the default spec: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("blank fallback = %+v, want the default %+v", got, want)
	}

	custom, err := defaultRetentionPolicy("daily=1")
	if err != nil {
		t.Fatalf("defaultRetentionPolicy(\"daily=1\"): %v", err)
	}
	if custom.Daily != 1 || custom.Weekly != 0 {
		t.Errorf("a provided spec should pass through wholesale, got %+v", custom)
	}
}
