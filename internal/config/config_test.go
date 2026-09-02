// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package config

import (
	"os"
	"testing"
)

// TestLoadDefaultsSplayOnWhenUnset proves the config-layer half of the
// BALLAST_SPLAY fix: Config.Splay is a *bool specifically so a config file
// (or environment) that never mentions splay at all still resolves to
// "splay on" after Load, not to the bool zero value (false), which would
// silently disable the anti-stampede splay by default.
func TestLoadDefaultsSplayOnWhenUnset(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Splay == nil {
		t.Fatal("Splay is nil after Load, want a non-nil default")
	}
	if !*cfg.Splay {
		t.Fatal("Splay = false after Load with no config or env, want true (splay defaults on)")
	}
}

// TestLoadHonorsBallastSplayFalse proves BALLAST_SPLAY=false actually
// reaches Config.Splay as an explicit false, not the zero-value ambiguity
// this pass fixed.
func TestLoadHonorsBallastSplayFalse(t *testing.T) {
	t.Setenv("BALLAST_SPLAY", "false")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Splay == nil || *cfg.Splay {
		t.Fatalf("Splay = %v, want explicit false from BALLAST_SPLAY=false", cfg.Splay)
	}
}

// TestLoadHonorsBallastSplayTrue proves an explicit BALLAST_SPLAY=true is
// indistinguishable in outcome from the default, but still goes through the
// same explicit-override path (not the nil-defaulting path).
func TestLoadHonorsBallastSplayTrue(t *testing.T) {
	t.Setenv("BALLAST_SPLAY", "true")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Splay == nil || !*cfg.Splay {
		t.Fatalf("Splay = %v, want explicit true from BALLAST_SPLAY=true", cfg.Splay)
	}
}

// TestLoadRejectsInvalidBallastSplay proves a malformed BALLAST_SPLAY value
// is rejected rather than silently ignored.
func TestLoadRejectsInvalidBallastSplay(t *testing.T) {
	t.Setenv("BALLAST_SPLAY", "not-a-bool")

	if _, err := Load(""); err == nil {
		t.Fatal("expected an error for an invalid BALLAST_SPLAY value")
	}
}

// TestLoadFileSplayFalseSurvivesNoEnvOverride proves a "splay: false" in
// the config file itself resolves to an explicit false when no
// BALLAST_SPLAY env var overrides it.
func TestLoadFileSplayFalseSurvivesNoEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/ballast.yml"
	if err := os.WriteFile(path, []byte("splay: false\n"), 0o644); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Splay == nil || *cfg.Splay {
		t.Fatalf("Splay = %v, want explicit false from the config file", cfg.Splay)
	}
}

// TestLoadLogFormatDefault proves log.format defaults to "text".
func TestLoadLogFormatDefault(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Format != "text" {
		t.Fatalf("Log.Format = %q, want text", cfg.Log.Format)
	}
}

// TestLoadLogFormatEnvOverride proves BALLAST_LOG_FORMAT overrides the file
// and default, and that an unknown value is rejected.
func TestLoadLogFormatEnvOverride(t *testing.T) {
	t.Setenv("BALLAST_LOG_FORMAT", "json")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Log.Format != "json" {
		t.Fatalf("Log.Format = %q, want json", cfg.Log.Format)
	}

	t.Setenv("BALLAST_LOG_FORMAT", "yaml")
	if _, err := Load(""); err == nil {
		t.Fatal("Load: expected error on invalid log.format, got nil")
	}
}
