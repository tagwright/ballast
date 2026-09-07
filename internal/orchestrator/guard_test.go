// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"strings"
	"testing"

	"github.com/tagwright/ballast/internal/record"
)

// writeSuccessfulRun persists one exit-0 run record for service under stateDir,
// through the real record.Write path, so the guard reads exactly what a real
// successful backup leaves behind.
func writeSuccessfulRun(t *testing.T, stateDir, service, runID string) {
	t.Helper()
	r := &record.Run{
		Record:  record.RecordType,
		RunID:   runID,
		Service: service,
		Exit:    0,
	}
	if _, err := record.Write(stateDir, r); err != nil {
		t.Fatalf("record.Write: %v", err)
	}
}

// TestGuardAutoInit_NewServiceAllows covers branch (a): a genuinely new service
// with no prior run history is allowed to auto-initialize (guard returns nil).
func TestGuardAutoInit_NewServiceAllows(t *testing.T) {
	stateDir := t.TempDir()

	if err := guardAutoInit(stateDir, "brand-new-service"); err != nil {
		t.Errorf("guardAutoInit for a new service = %v, want nil (auto-init allowed)", err)
	}
}

// TestGuardAutoInit_PriorBackupsRefuses covers branch (b): a service that has
// backed up successfully before, whose destination is now uninitialized, is
// refused (guard returns a non-nil regression error) rather than silently
// re-initialized.
func TestGuardAutoInit_PriorBackupsRefuses(t *testing.T) {
	stateDir := t.TempDir()
	writeSuccessfulRun(t, stateDir, "gitea", "01J000000000000000000000A0")
	writeSuccessfulRun(t, stateDir, "gitea", "01J000000000000000000000A1")

	err := guardAutoInit(stateDir, "gitea")
	if err == nil {
		t.Fatal("guardAutoInit with prior successful backups = nil, want a regression error")
	}
	// The message must name the service and the count, so the failure
	// notification says which service lost its backups and how many.
	msg := err.Error()
	if !strings.Contains(msg, "gitea") {
		t.Errorf("error %q does not name the service", msg)
	}
	if !strings.Contains(msg, "2 prior successful") {
		t.Errorf("error %q does not report the prior-backup count", msg)
	}
	if !strings.Contains(msg, "refusing to silently re-initialize") {
		t.Errorf("error %q does not state that it is refusing to re-initialize", msg)
	}
}

// TestGuardAutoInit_OnlyFailedRunsAllows guards against a false positive: a
// service whose only history is failed runs never took a real backup, so
// nothing was lost and auto-init must still be allowed.
func TestGuardAutoInit_OnlyFailedRunsAllows(t *testing.T) {
	stateDir := t.TempDir()
	r := &record.Run{
		Record:  record.RecordType,
		RunID:   "01J000000000000000000000B0",
		Service: "flaky",
		Exit:    1,
	}
	if _, err := record.Write(stateDir, r); err != nil {
		t.Fatalf("record.Write: %v", err)
	}

	if err := guardAutoInit(stateDir, "flaky"); err != nil {
		t.Errorf("guardAutoInit with only failed prior runs = %v, want nil", err)
	}
}

// TestGuardAutoInit_NoStateDirAllows preserves the original behavior when state
// recording is off: with no state directory there is no history to consult, so
// the guard must not refuse an init.
func TestGuardAutoInit_NoStateDirAllows(t *testing.T) {
	if err := guardAutoInit("", "any-service"); err != nil {
		t.Errorf("guardAutoInit with no state dir = %v, want nil (behavior preserved)", err)
	}
}
