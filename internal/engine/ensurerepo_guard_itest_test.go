// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

//go:build integration

// Package engine's ensurerepo_guard_itest_test.go is excluded from the normal
// "go test ./..." run (see docs/TESTING.md's "Unit tests" description: no live
// socket, no external service, no real restic binary). It requires a real
// restic binary on PATH and is run by test/integration/run-ensurerepo-guard.sh,
// which builds a throwaway container carrying one.
//
// It proves, end to end against a real restic repository, the auto-init guard
// EnsureRepo consults on the would-init path (Repo.GuardInit): a genuinely new
// service auto-initializes as before, while a service whose guard refuses the
// init (the regression case: it has backed up before but its destination is now
// gone) fails loudly and leaves no fresh repository behind. The decision logic
// that feeds the guard in production is unit-tested without restic in
// internal/record (CountSuccessfulRuns) and internal/orchestrator
// (guardAutoInit); this test proves the engine actually honors it against real
// restic init behavior.
package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// guardItestPassword is a fixed password for this test's throwaway repository.
// It is never a real credential (the repository lives under t.TempDir() and is
// destroyed with the test), so there is no reason to derive or generate one.
const guardItestPassword = "ballast-itest-ensurerepo-guard-password"

// repoInitialized reports whether repoDir holds an initialized restic
// repository, by the presence of its top-level "config" object that
// `restic init` writes. Checked directly on the filesystem rather than through
// restic so the "not initialized" assertion cannot itself be confused by a
// restic error.
func repoInitialized(repoDir string) bool {
	_, err := os.Stat(filepath.Join(repoDir, "config"))
	return err == nil
}

// TestEnsureRepoGuard exercises both branches of EnsureRepo's auto-init guard
// against a real restic binary.
func TestEnsureRepoGuard(t *testing.T) {
	if _, err := exec.LookPath("restic"); err != nil {
		t.Skip("restic not found on PATH; run via test/integration/run-ensurerepo-guard.sh")
	}

	ctx := context.Background()
	r := NewRestic("")

	// Branch (a): a genuinely new service (nil guard, so nothing refuses the
	// init) auto-initializes exactly as before.
	t.Run("new service auto-inits", func(t *testing.T) {
		repoDir := filepath.Join(t.TempDir(), "repo")
		repo := Repo{
			URL:      repoDir,
			Password: func() (string, error) { return guardItestPassword, nil },
			// GuardInit nil: preserves the original always-init behavior.
		}

		if repoInitialized(repoDir) {
			t.Fatalf("precondition: repo dir %q already initialized", repoDir)
		}
		if err := r.EnsureRepo(ctx, repo); err != nil {
			t.Fatalf("EnsureRepo on a new service = %v, want nil (auto-init)", err)
		}
		if !repoInitialized(repoDir) {
			t.Errorf("repo %q not initialized after EnsureRepo; auto-init did not run", repoDir)
		}
	})

	// Branch (b): a service whose guard refuses the init (the regression case)
	// gets that error surfaced verbatim, and no fresh repository is created.
	t.Run("guard refuses re-init and does not create a repo", func(t *testing.T) {
		repoDir := filepath.Join(t.TempDir(), "repo")
		regression := errors.New("destination missing but prior backups exist; refusing to silently re-initialize")
		guardCalled := false
		repo := Repo{
			URL:      repoDir,
			Password: func() (string, error) { return guardItestPassword, nil },
			GuardInit: func() error {
				guardCalled = true
				return regression
			},
		}

		if repoInitialized(repoDir) {
			t.Fatalf("precondition: repo dir %q already initialized", repoDir)
		}
		err := r.EnsureRepo(ctx, repo)
		if !guardCalled {
			t.Error("GuardInit was never consulted on the would-init path")
		}
		if !errors.Is(err, regression) {
			t.Fatalf("EnsureRepo error = %v, want the guard's refusal error", err)
		}
		if repoInitialized(repoDir) {
			t.Errorf("repo %q was initialized despite the guard refusing; the safeguard did not hold", repoDir)
		}
	})
}
