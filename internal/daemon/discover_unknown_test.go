// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"strings"
	"testing"

	"github.com/tagwright/beacon"
	"github.com/tagwright/core/runtime"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
)

// #600(b): a container carrying an unrecognized ballast label suffix must be
// backed up under its recognized labels AND draw a loud operator alert naming
// the suffix. This is the warn-and-continue contract that replaced the earlier
// fail-closed skip: a typo must never silently cost a working backup, but it
// must never pass unheard either.
func TestDiscoverOne_UnknownSuffixAlertsButStillRegisters(t *testing.T) {
	notifier, cap := captureNotifier(t)

	sched, err := schedule.New(schedule.Config{})
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	cfg := &config.Config{
		Destinations:       map[string]config.Destination{"local": {URL: "/repos"}},
		DefaultDestination: "local",
		Schedule:           "@daily",
	}
	deps := orchestrator.Deps{Config: cfg}
	reg := newRegistry()

	c := runtime.Container{
		ID:   "c1",
		Name: "/webapp",
		Labels: map[string]string{
			"ballast.enable":        "true",
			"ballast.retenton.last": "7", // typo for retention.last
		},
	}

	discoverOne(c, cfg, reg, sched, deps, discardLog(), notifier)

	// Still backed up: the service was registered, not skipped over the typo.
	if got := len(reg.specs()); got != 1 {
		t.Fatalf("registry has %d specs, want 1 (an unknown suffix must not skip the backup)", got)
	}

	// Loud: exactly one Warning-level alert naming the unrecognized suffix.
	notes := cap.notifications()
	if len(notes) != 1 {
		t.Fatalf("an unknown suffix fired %d notifications, want exactly 1 (it must reach the operator)", len(notes))
	}
	n := notes[0]
	if n.Level != beacon.LevelWarning {
		t.Errorf("unknown-suffix alert level = %v, want LevelWarning", n.Level)
	}
	if !strings.Contains(n.Body, "retenton.last") {
		t.Errorf("unknown-suffix alert body = %q, want it to name the offending suffix", n.Body)
	}
}
