// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/secret"
	"github.com/tagwright/core/runtime/runtimetest"
)

// testMaster is a SYNTHETIC, non-secret master used to exercise the master
// resolution path. It is the same synthetic value the secret package's frozen
// golden test uses, so repoPasswordGolden below is that construction's pinned
// output for the "photos-db" service. It is comfortably over LoadMaster's
// 32-byte floor.
const testMaster = "synthetic-test-master-for-hkdf-golden-vectors-01"

// repoPasswordGolden is DeriveRepoPassword(testMaster, "photos-db") under the
// FROZEN v1 HKDF construction, pinned here so a change to WHEN the master is
// resolved can never quietly change WHAT password is derived. It is copied
// from the secret package's own golden vector for the same (master, service)
// pair.
const repoPasswordGolden = "O-awKj43w9PfdLSnfpAV9XX1H8wd6U0trG93ljBsnGE"

// mutableSecrets is a Resolver-backed secret store a test can mutate between
// runs, so a single long-lived Deps can see a master that was absent at one
// run and present at a later one. It models the daemon's real world: the
// secrets directory is a live mount whose contents change under a running
// process.
type mutableSecrets struct {
	mu     sync.Mutex
	values map[string]string
}

func newMutableSecrets() *mutableSecrets {
	return &mutableSecrets{values: map[string]string{}}
}

func (m *mutableSecrets) set(name, value string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] = value
}

// resolve is the secret.Resolver seam. A missing secret returns an error, the
// same shape FileEnvResolver returns for a name that resolves to neither a
// file nor an env var, so LoadMaster wraps it the same way in a test as in
// production.
func (m *mutableSecrets) resolve(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.values[name]
	if !ok {
		return "", &notFoundError{name: name}
	}
	return v, nil
}

type notFoundError struct{ name string }

func (e *notFoundError) Error() string {
	return "secret: " + e.name + " not found"
}

// passwordProbeEngine embeds okEngine (all ops succeed) but overrides
// EnsureRepo to actually CALL the repo's password closure and return its
// error. This is what makes a RunBackup exercise master resolution end to
// end: the closure is only evaluated when the engine needs the password, so a
// test that drives RunBackup through this engine proves the master is
// resolved per run, not once at BuildRepo time.
type passwordProbeEngine struct{ okEngine }

func (passwordProbeEngine) EnsureRepo(_ context.Context, repo engine.Repo) error {
	_, err := repo.Password()
	return err
}

func selfHealDeps(resolver secret.Resolver) Deps {
	return Deps{
		Runtime: runtimetest.New(),
		Engine:  passwordProbeEngine{},
		Config: &config.Config{
			Runtime:      "docker",
			Retention:    "daily=7",
			Destinations: map[string]config.Destination{"local": {URL: "/repos"}},
		},
		Resolver: resolver,
	}
}

// TestRunBackup_MasterAbsentFailsLoudly_ThenSelfHeals is the regression guard
// for the whole reload-per-run fix. It drives the REAL RunBackup path twice
// with ONE long-lived Deps instance (never recreated between runs), which is
// exactly the daemon's situation: a process started while the master secret
// was absent.
//
//  1. The first run happens with no master provisioned and must fail LOUDLY:
//     the existing load error surfaces as the run's error, never a silent
//     success.
//  2. The master is then provisioned into the same secret store, and a second
//     run over the same Deps must SUCCEED with no restart. If BuildRepo
//     snapshotted the master once (the old behavior), this run would still
//     fail; that it succeeds is the self-heal the fix exists to give.
func TestRunBackup_MasterAbsentFailsLoudly_ThenSelfHeals(t *testing.T) {
	secrets := newMutableSecrets() // master deliberately absent
	deps := selfHealDeps(secrets.resolve)
	spec := &discovery.BackupSpec{Service: "svc", Destination: "local", RepoPath: "svc"}

	// Run 1: master absent -> loud failure carrying the load error.
	err := RunBackup(context.Background(), spec, deps)
	if err == nil {
		t.Fatal("run with no master succeeded silently; it must fail loudly until a master is provisioned")
	}
	if !strings.Contains(err.Error(), "load master") {
		t.Fatalf("run-1 error does not surface the master-load failure: %v", err)
	}

	// Provision the master into the SAME store the SAME Deps already holds a
	// resolver over. No Deps/orchestrator reconstruction.
	secrets.set(secret.MasterSecretName, testMaster)

	// Run 2: same long-lived Deps -> self-heals and succeeds.
	if err := RunBackup(context.Background(), spec, deps); err != nil {
		t.Fatalf("run after the master returned still failed; the daemon did not self-heal: %v", err)
	}
}

// TestBuildRepo_PasswordSecretOverrideWorksWithoutMaster proves the granular
// degradation the lazy-in-closure approach buys: a service with an explicit
// ballast.password-secret override never touches the master, so its repo
// password resolves even while the master is absent. Only master-derived
// services degrade when the master is missing, not every service.
func TestBuildRepo_PasswordSecretOverrideWorksWithoutMaster(t *testing.T) {
	secrets := newMutableSecrets() // master absent
	secrets.set("svc-repo-pw", "an-explicit-per-service-password")

	spec := &discovery.BackupSpec{
		Service:        "svc",
		Destination:    "local",
		RepoPath:       "svc",
		PasswordSecret: "svc-repo-pw",
	}
	cfg := &config.Config{Destinations: map[string]config.Destination{"local": {URL: "/repos"}}}

	repo, err := BuildRepo(spec, cfg, secrets.resolve)
	if err != nil {
		t.Fatalf("BuildRepo: %v", err)
	}

	pw, err := repo.Password()
	if err != nil {
		t.Fatalf("password-secret override should resolve with no master present, got: %v", err)
	}
	if pw != "an-explicit-per-service-password" {
		t.Fatalf("password-secret override resolved to %q, want the overriding secret's value", pw)
	}
}

// TestBuildRepo_MasterDerivationUnchanged proves the change touched only WHEN
// the master is read, never HOW the password is derived: the closure's output
// for a known (master, service) pair is byte-identical to both a direct call
// of the frozen DeriveRepoPassword and the pinned golden vector for the frozen
// v1 construction. If this fails, the derivation moved, not just its timing.
func TestBuildRepo_MasterDerivationUnchanged(t *testing.T) {
	secrets := newMutableSecrets()
	secrets.set(secret.MasterSecretName, testMaster)

	spec := &discovery.BackupSpec{Service: "photos-db", Destination: "local", RepoPath: "photos-db"}
	cfg := &config.Config{Destinations: map[string]config.Destination{"local": {URL: "/repos"}}}

	repo, err := BuildRepo(spec, cfg, secrets.resolve)
	if err != nil {
		t.Fatalf("BuildRepo: %v", err)
	}

	pw, err := repo.Password()
	if err != nil {
		t.Fatalf("repo.Password() with a valid master: %v", err)
	}

	direct, err := secret.DeriveRepoPassword([]byte(testMaster), "photos-db")
	if err != nil {
		t.Fatalf("DeriveRepoPassword: %v", err)
	}
	if pw != direct {
		t.Fatalf("closure-derived password %q != direct DeriveRepoPassword %q; the derivation path changed", pw, direct)
	}
	if pw != repoPasswordGolden {
		t.Fatalf("closure-derived password %q != frozen v1 golden %q; the frozen HKDF construction appears to have changed", pw, repoPasswordGolden)
	}
}
