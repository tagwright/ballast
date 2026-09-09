// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package verify

import "testing"

// TestExpectMatches is the #519 regression: a bare expect is a LITERAL
// substring (never accidentally a regex), and regex is opt-in via "re:".
func TestExpectMatches(t *testing.T) {
	cases := []struct {
		name         string
		expect, out  string
		want         bool
	}{
		// #519 false-pass cases: a literal expect must NOT match a different
		// string just because it is also a valid regex.
		{"literal dot does not match any-char", "1.0", "1X0", false},
		{"bare dot does not match unrelated output", ".", "garbage-unrelated", false},
		// Literal positives: the substring is actually present.
		{"literal token present", "VERIFY_OK", "row\nVERIFY_OK\n", true},
		{"literal version present verbatim", "1.0", "version 1.0 ok", true},
		{"literal absent", "VERIFY_OK", "VERIFY_FAIL", false},
		// Regex opt-in via re:.
		{"re rowcount matches a positive integer", "re:^[1-9][0-9]*$", "79", true},
		{"re rowcount rejects zero", "re:^[1-9][0-9]*$", "0", false},
		{"re rowcount rejects empty", "re:^[1-9][0-9]*$", "", false},
		{"re anchored does not match a superset line", "re:^ok$", "not ok", false},
		// An explicit but invalid regex matches nothing (fail closed), never
		// falls back to a substring that could accidentally match.
		{"re invalid pattern fails closed", "re:[unterminated(", "[unterminated(", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := expectMatches(c.expect, c.out); got != c.want {
				t.Errorf("expectMatches(%q, %q) = %v, want %v", c.expect, c.out, got, c.want)
			}
		})
	}
}
