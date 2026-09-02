// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

//go:build integration

// Package engine's forget_time_itest_test.go is excluded from the normal
// "go test ./..." run (see docs/TESTING.md's "Unit tests" description: no
// live socket, no external service, no real restic binary). It requires a
// real restic binary on PATH and is run by
// test/integration/run-retention-time.sh, which builds a throwaway
// container carrying one.
//
// It proves what test/integration/run-retention.sh's TestKeepLast-style
// assertion structurally cannot: time-based retention (keep-daily and
// friends). Ballast's own BackupRequest has no way to backdate a
// snapshot's timestamp (nor should it -- that is exactly the kind of thing
// a real backup client should never let an operator forge for a real
// backup), so there is no way to prove keep-daily's bucketing through
// Ballast's own CLI or a real elapsed-time itest. Restic itself, though,
// exposes exactly the escape hatch this needs: `restic backup --time`.
// This test uses that directly (bypassing Engine.Backup, which does not
// expose --time) to seed a repository with snapshots at controlled,
// synthetic times, then exercises the exact production code path for the
// retention side: engine.Restic.Forget, called through the Engine
// interface precisely as internal/orchestrator/backup.go's
// runBackupSteps calls it.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// itestRepoPassword is a fixed password for this test's throwaway
// repository. It is never a real credential (the repository is destroyed
// with the test's t.TempDir()), so there is no reason to derive or
// generate one.
const itestRepoPassword = "ballast-itest-forget-time-password"

// seedSnapshot runs `restic backup --time <t> --tag <tag> <dataDir>`
// directly against repoDir, bypassing Engine.Backup entirely (it has no
// --time equivalent by design). This is the one place in this test that
// does NOT go through Ballast's own Engine interface: everything after
// seeding (EnsureRepo, Snapshots, Forget) does.
func seedSnapshot(t *testing.T, repoDir, dataDir string, at time.Time, tag string) {
	t.Helper()

	// restic's --time flag parses a fixed "2006-01-02 15:04:05" layout (no
	// "T" separator, no zone offset) as a LOCAL time; TZ=UTC below pins that
	// interpretation (and Forget's own daily-bucket evaluation, which also
	// runs in local time) to UTC, matching the UTC clock this test's times
	// are all computed in.
	restic := at.Format("2006-01-02 15:04:05")
	cmd := exec.Command("restic", "backup", "--time", restic, "--tag", tag, dataDir)
	cmd.Env = append(os.Environ(),
		"RESTIC_REPOSITORY="+repoDir,
		"RESTIC_PASSWORD="+itestRepoPassword,
		"TZ=UTC",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("restic backup --time %q --tag %s: %v\nstderr: %s", restic, tag, err, stderr.String())
	}
}

// snapshotIDsByTag lists repoDir's snapshots (via the real Engine
// interface, exactly like production code would) and returns a tag ->
// snapshot-ID map. Every seeded snapshot in this test carries exactly one
// tag, so a 1:1 map is unambiguous.
func snapshotIDsByTag(t *testing.T, ctx context.Context, r *Restic, repo Repo) map[string]string {
	t.Helper()

	snaps, err := r.Snapshots(ctx, repo)
	if err != nil {
		t.Fatalf("Snapshots: %v", err)
	}
	out := make(map[string]string, len(snaps))
	for _, s := range snaps {
		if len(s.Tags) != 1 {
			t.Fatalf("snapshot %s has %d tags, want exactly 1 (%v)", s.ID, len(s.Tags), s.Tags)
		}
		out[s.Tags[0]] = s.ID
	}
	return out
}

// TestForgetKeepDailyTimeBased seeds a repository with six snapshots across
// five distinct calendar days (two on one day, to also prove the
// same-day-keeps-only-the-newest bucketing rule, not just a count), applies
// RetentionPolicy{Daily: 3} through the real engine.Restic.Forget, and
// asserts the exact surviving snapshot set: the three most recent distinct
// days (today, yesterday, the day before), with only the LATER of the two
// same-day snapshots surviving from that middle day.
func TestForgetKeepDailyTimeBased(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not found on PATH; run via test/integration/run-retention-time.sh")
	}

	// Pin every restic subprocess's day-bucket evaluation to UTC (both the
	// seeded --time backups above and Forget's own daily-bucket evaluation
	// below run in whatever the child process's local time zone is), so it
	// matches the UTC clock this test's synthetic times are computed in
	// regardless of the host or container's actual configured zone. This
	// also reaches engine.Restic.run's childEnv, which builds each Forget
	// subprocess's environment from this test process's own os.Environ().
	t.Setenv("TZ", "UTC")

	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "repo")
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(dataDir): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "file.txt"), []byte("ballast itest forget-time payload\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx := context.Background()
	r := NewRestic("")
	repo := Repo{
		URL:      repoDir,
		Password: func() (string, error) { return itestRepoPassword, nil },
	}

	if err := r.EnsureRepo(ctx, repo); err != nil {
		t.Fatalf("EnsureRepo: %v", err)
	}

	// Anchor on real "now" and walk backward in whole days, so the test
	// passes regardless of what day it actually runs on. Each time is
	// mid-morning UTC, comfortably clear of any day-boundary edge case;
	// the D2AM/D2PM pair on the middle day is what proves the
	// keep-one-per-day bucketing rule, not merely a count.
	now := time.Now().UTC()
	dayStart := func(daysAgo int) time.Time {
		d := now.AddDate(0, 0, -daysAgo)
		return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	}

	type seed struct {
		tag string
		at  time.Time
	}
	seeds := []seed{
		{"D4", dayStart(4).Add(10 * time.Hour)},
		{"D3", dayStart(3).Add(10 * time.Hour)},
		{"D2AM", dayStart(2).Add(8 * time.Hour)},
		{"D2PM", dayStart(2).Add(20 * time.Hour)},
		{"D1", dayStart(1).Add(10 * time.Hour)},
		{"D0", dayStart(0).Add(10 * time.Hour)},
	}
	for _, s := range seeds {
		seedSnapshot(t, repoDir, dataDir, s.at, s.tag)
	}

	before := snapshotIDsByTag(t, ctx, r, repo)
	if len(before) != len(seeds) {
		t.Fatalf("seeded %d snapshots but Snapshots reports %d: %v", len(seeds), len(before), before)
	}
	t.Logf("seeded snapshots: %s", mustJSON(before))

	if err := r.Forget(ctx, repo, RetentionPolicy{Daily: 3}); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	after := snapshotIDsByTag(t, ctx, r, repo)
	t.Logf("surviving snapshots: %s", mustJSON(after))

	wantSurvive := map[string]bool{"D0": true, "D1": true, "D2PM": true}
	wantForgotten := map[string]bool{"D4": true, "D3": true, "D2AM": true}

	for tag := range wantSurvive {
		if _, ok := after[tag]; !ok {
			t.Errorf("expected %q to survive keep-daily=3, but it was forgotten", tag)
		}
		if before[tag] != after[tag] {
			t.Errorf("surviving snapshot %q changed ID from %s to %s", tag, before[tag], after[tag])
		}
	}
	for tag := range wantForgotten {
		if id, ok := after[tag]; ok {
			t.Errorf("expected %q to be forgotten by keep-daily=3, but it survived as %s", tag, id)
		}
	}
	if len(after) != len(wantSurvive) {
		t.Errorf("surviving snapshot count = %d, want %d (exactly %v)", len(after), len(wantSurvive), wantSurvive)
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<marshal error: %v>", err)
	}
	return string(b)
}
