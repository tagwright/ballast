// SPDX-License-Identifier: GPL-3.0-or-later

package ulid

import (
	"regexp"
	"testing"
)

// frozen is the ulid pattern from schema/ballast/common.v1.json.
var frozen = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)

func TestNewMatchesFrozenPattern(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !frozen.MatchString(id) {
			t.Fatalf("id %q does not match frozen ulid pattern", id)
		}
	}
}

func TestNewUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 10000; i++ {
		id, err := New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if seen[id] {
			t.Fatalf("duplicate ulid %q", id)
		}
		seen[id] = true
	}
}

func TestEncodeLength(t *testing.T) {
	var b [16]byte
	if got := len(encode(b)); got != 26 {
		t.Fatalf("encode length = %d, want 26", got)
	}
	// All-zero input encodes to all-zero Crockford characters.
	if got := encode(b); got != "00000000000000000000000000" {
		t.Fatalf("zero encode = %q", got)
	}
}
