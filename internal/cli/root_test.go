// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package cli

import "testing"

// TestResolveConfigPath pins the flag-vs-BALLAST_CONFIG precedence that lets a
// `docker exec` into the daemon container reach the same ballast.yml the daemon
// uses without repeating --config.
func TestResolveConfigPath(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("BALLAST_CONFIG", "/from/env.yml")
		if got := resolveConfigPath("/from/flag.yml"); got != "/from/flag.yml" {
			t.Fatalf("resolveConfigPath = %q, want the explicit flag value", got)
		}
	})
	t.Run("env used when flag empty", func(t *testing.T) {
		t.Setenv("BALLAST_CONFIG", "/from/env.yml")
		if got := resolveConfigPath(""); got != "/from/env.yml" {
			t.Fatalf("resolveConfigPath = %q, want the BALLAST_CONFIG value", got)
		}
	})
	t.Run("empty when neither is set", func(t *testing.T) {
		t.Setenv("BALLAST_CONFIG", "")
		if got := resolveConfigPath(""); got != "" {
			t.Fatalf("resolveConfigPath = %q, want empty (env-only operation)", got)
		}
	})
}
