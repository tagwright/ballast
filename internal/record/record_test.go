// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package record

import (
	"os"
	"path/filepath"
	"testing"
)

// writeRun persists a minimal run record for service with the given run id and
// exit code, through the same Write path production uses, so the count under
// test reads exactly what a real run leaves behind.
func writeRun(t *testing.T, stateDir, service, runID string, exit int) {
	t.Helper()
	r := &Run{
		Record:  RecordType,
		RunID:   runID,
		Service: service,
		Exit:    exit,
	}
	if _, err := Write(stateDir, r); err != nil {
		t.Fatalf("Write(%s/%s): %v", service, runID, err)
	}
}

func TestCountSuccessfulRuns_NoHistory(t *testing.T) {
	dir := t.TempDir()

	// A service that has never run has no runs/<service>/ directory at all.
	// This is the ordinary new-service case and must count as zero, with no
	// error, so its first backup is allowed to auto-initialize.
	n, err := CountSuccessfulRuns(dir, "brand-new-service")
	if err != nil {
		t.Fatalf("CountSuccessfulRuns on missing history: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 for a service with no run history", n)
	}
}

func TestCountSuccessfulRuns_CountsOnlyExitZero(t *testing.T) {
	dir := t.TempDir()

	// Two successful runs (exit 0) and one failed run (exit 1) for the service.
	writeRun(t, dir, "gitea", "01J000000000000000000000A0", 0)
	writeRun(t, dir, "gitea", "01J000000000000000000000A1", 1)
	writeRun(t, dir, "gitea", "01J000000000000000000000A2", 0)

	// A different service's records must not leak into gitea's count.
	writeRun(t, dir, "nextcloud", "01J000000000000000000000B0", 0)

	n, err := CountSuccessfulRuns(dir, "gitea")
	if err != nil {
		t.Fatalf("CountSuccessfulRuns: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (only the exit-0 records for gitea)", n)
	}
}

func TestCountSuccessfulRuns_OnlyFailures(t *testing.T) {
	dir := t.TempDir()

	// A service whose only history is failed runs never took a real backup, so
	// nothing was lost: it must count as zero and be allowed to auto-init.
	writeRun(t, dir, "flaky", "01J000000000000000000000C0", 1)
	writeRun(t, dir, "flaky", "01J000000000000000000000C1", 1)

	n, err := CountSuccessfulRuns(dir, "flaky")
	if err != nil {
		t.Fatalf("CountSuccessfulRuns: %v", err)
	}
	if n != 0 {
		t.Errorf("count = %d, want 0 when every prior run failed", n)
	}
}

func TestCountSuccessfulRuns_SkipsUnparseableAndNonJSON(t *testing.T) {
	dir := t.TempDir()

	writeRun(t, dir, "svc", "01J000000000000000000000D0", 0)

	runsDir := filepath.Join(dir, "runs", "svc")
	// A half-written / corrupt record file must be skipped, not fail the count.
	if err := os.WriteFile(filepath.Join(runsDir, "corrupt.json"), []byte("{not valid json"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-.json file (a stray temp file, say) must be ignored entirely.
	if err := os.WriteFile(filepath.Join(runsDir, "notes.txt"), []byte("exit 0"), 0o644); err != nil {
		t.Fatal(err)
	}

	n, err := CountSuccessfulRuns(dir, "svc")
	if err != nil {
		t.Fatalf("CountSuccessfulRuns: %v", err)
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (corrupt and non-json entries skipped)", n)
	}
}
