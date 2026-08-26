// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/core/runtime"
)

// TestDiscoverPrefixConflictReturnsSpecAlongsideError proves a
// ballast.*/tagwright.backup.* prefix conflict (the same suffix set to
// different values under each prefix) is surfaced as a non-nil spec plus a
// non-nil error, not a nil spec: every caller that matches a container by
// BackupSpec.Service before checking the error (internal/cli's backup.go
// service loop and deps.go's discoverService) needs a spec with a resolved
// Service to find this container at all and report the real conflict,
// instead of every such call site's "if spec == nil { continue }" silently
// skipping past it and reporting a misleading "service not found".
func TestDiscoverPrefixConflictReturnsSpecAlongsideError(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load(\"\"): %v", err)
	}

	c := runtime.Container{
		ID:   "c1",
		Name: "conflicted",
		Labels: map[string]string{
			"ballast.enable":        "true",
			"ballast.repo":          "A",
			"tagwright.backup.repo": "B",
		},
	}

	spec, _, err := Discover(c, cfg)
	if err == nil {
		t.Fatal("Discover: expected a conflict error, got nil")
	}
	if spec == nil {
		t.Fatal("Discover: spec is nil, want a best-effort spec alongside the conflict error")
	}
	if spec.Service != "conflicted" {
		t.Errorf("spec.Service = %q, want %q (the container-name fallback, since ballast.name could not be read)", spec.Service, "conflicted")
	}
	if spec.ContainerID != "c1" {
		t.Errorf("spec.ContainerID = %q, want %q", spec.ContainerID, "c1")
	}
}

// TestDiscoverGlobalExcludeMergesWithLabel proves config.Config.Exclude (the
// global glob-exclude list, documented as "additive to any per-service
// ballast.exclude labels") actually reaches BackupSpec.Excludes alongside
// the service's own ballast.exclude patterns, rather than being silently
// dropped.
func TestDiscoverGlobalExcludeMergesWithLabel(t *testing.T) {
	path := writeTempConfig(t, "exclude:\n  - \"*.tmp\"\n")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}

	c := runtime.Container{
		ID:   "c1",
		Name: "app",
		Labels: map[string]string{
			"ballast.enable":  "true",
			"ballast.exclude": "*.log",
		},
	}

	spec, _, err := Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got := map[string]bool{}
	for _, p := range spec.Excludes {
		got[p] = true
	}
	if !got["*.tmp"] {
		t.Errorf("spec.Excludes = %v, want to contain the global %q", spec.Excludes, "*.tmp")
	}
	if !got["*.log"] {
		t.Errorf("spec.Excludes = %v, want to contain the per-service %q", spec.Excludes, "*.log")
	}
	if len(spec.Excludes) != 2 {
		t.Errorf("spec.Excludes = %v, want exactly 2 entries", spec.Excludes)
	}
}

// TestDiscoverNoGlobalExcludeKeepsLabelOnly proves an empty global Exclude
// list leaves a service's own ballast.exclude untouched (no accidental nil
// entries or duplication from the merge).
func TestDiscoverNoGlobalExcludeKeepsLabelOnly(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load(\"\"): %v", err)
	}

	c := runtime.Container{
		ID:   "c1",
		Name: "app",
		Labels: map[string]string{
			"ballast.enable":  "true",
			"ballast.exclude": "*.log",
		},
	}

	spec, _, err := Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(spec.Excludes) != 1 || spec.Excludes[0] != "*.log" {
		t.Fatalf("spec.Excludes = %v, want [\"*.log\"]", spec.Excludes)
	}
}
