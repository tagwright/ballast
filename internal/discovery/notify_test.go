// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package discovery

import (
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/core/runtime"
)

// TestDiscoverNotifyLabelsDefault proves both notify labels default to false
// when absent, matching every other optional bool label in the grammar.
func TestDiscoverNotifyLabelsDefault(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load(\"\"): %v", err)
	}

	c := runtime.Container{
		ID:     "c1",
		Name:   "app",
		Labels: map[string]string{"ballast.enable": "true"},
	}

	spec, _, err := Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if spec.NotifySuppress {
		t.Error("spec.NotifySuppress = true, want false (default)")
	}
	if spec.NotifyOnSuccess {
		t.Error("spec.NotifyOnSuccess = true, want false (default)")
	}
}

// TestDiscoverNotifyLabelsSet proves ballast.notify.suppress and
// ballast.notify.on-success are parsed into the corresponding BackupSpec
// fields, and that the tagwright.backup. alias reaches the same fields since
// both go through the same prefix-stripping label reader.
func TestDiscoverNotifyLabelsSet(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load(\"\"): %v", err)
	}

	c := runtime.Container{
		ID:   "c2",
		Name: "app",
		Labels: map[string]string{
			"ballast.enable":                     "true",
			"ballast.notify.suppress":            "true",
			"tagwright.backup.notify.on-success": "true",
		},
	}

	spec, _, err := Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !spec.NotifySuppress {
		t.Error("spec.NotifySuppress = false, want true")
	}
	if !spec.NotifyOnSuccess {
		t.Error("spec.NotifyOnSuccess = false, want true")
	}
}
