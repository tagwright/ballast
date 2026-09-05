// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package secret

import "testing"

// These golden values pin the FROZEN v1 HKDF construction described in the
// CONTRACT block at the top of derive.go: SHA-256, the fixed salt
// "tagwright.ballast.repo-key.v1", info "service:<name>", a 32-byte output,
// base64.RawURLEncoding. goldenMaster is a SYNTHETIC, non-secret test master:
// a readable placeholder string, deliberately not a real-looking key. The
// goldenVector* values are its derived outputs, computed once with the exact
// code in this package and hardcoded here as a regression tripwire.
//
// If this test ever starts failing, do NOT "fix" it by updating the
// constants to match new output: that means the frozen contract changed,
// which orphans every repository already built on the old derivation.
// Diagnose why the derivation moved, not why the test is "wrong".
const (
	goldenMaster = "synthetic-test-master-for-hkdf-golden-vectors-01"

	goldenVectorPhotosDB    = "O-awKj43w9PfdLSnfpAV9XX1H8wd6U0trG93ljBsnGE"
	goldenVectorPhotosMedia = "2uHqHhAmEi54WY0o-UFWOxmnGFLsj5arj_C4YsE4sXE"
	goldenVectorTimeTracker = "lZhPsZQK4PtN-ThZOwXdhiH4-BdhHnM3kJOcQuYE7eY"
)

// TestDeriveRepoPasswordGoldenValues pins DeriveRepoPassword's output for a
// fixed master and several service names against values captured once with
// this exact code path. A pass proves nothing has drifted; a failure means
// the salt, info template, output length, or encoding in derive.go changed
// out from under the frozen v1 contract.
func TestDeriveRepoPasswordGoldenValues(t *testing.T) {
	master := []byte(goldenMaster)

	cases := []struct {
		service string
		want    string
	}{
		{"photos-db", goldenVectorPhotosDB},
		{"photos-media", goldenVectorPhotosMedia},
		{"timetracker", goldenVectorTimeTracker},
	}

	for _, c := range cases {
		got, err := DeriveRepoPassword(master, c.service)
		if err != nil {
			t.Fatalf("DeriveRepoPassword(master, %q): unexpected error: %v", c.service, err)
		}
		if got != c.want {
			t.Errorf("DeriveRepoPassword(master, %q) = %q, want pinned golden value %q (the frozen v1 HKDF construction appears to have changed)",
				c.service, got, c.want)
		}
	}
}

// TestDeriveRepoPasswordDeterministic proves the same (master, service) pair
// always derives the same password: the property "ballast key <service>"
// depends on to recover a repository password with no stored per-service
// state.
func TestDeriveRepoPasswordDeterministic(t *testing.T) {
	master := []byte(goldenMaster)

	first, err := DeriveRepoPassword(master, "photos-db")
	if err != nil {
		t.Fatalf("DeriveRepoPassword: %v", err)
	}
	second, err := DeriveRepoPassword(master, "photos-db")
	if err != nil {
		t.Fatalf("DeriveRepoPassword: %v", err)
	}
	if first != second {
		t.Errorf("DeriveRepoPassword(master, %q) is not deterministic: %q != %q", "photos-db", first, second)
	}
}

// TestDeriveRepoPasswordDistinctPerService proves two different service
// names derive different passwords from the same master, i.e. that the info
// parameter actually domain-separates services from each other rather than
// collapsing to a single shared repository password.
func TestDeriveRepoPasswordDistinctPerService(t *testing.T) {
	master := []byte(goldenMaster)

	a, err := DeriveRepoPassword(master, "photos-db")
	if err != nil {
		t.Fatalf("DeriveRepoPassword: %v", err)
	}
	b, err := DeriveRepoPassword(master, "photos-media")
	if err != nil {
		t.Fatalf("DeriveRepoPassword: %v", err)
	}
	if a == b {
		t.Errorf("DeriveRepoPassword produced the same password %q for two different service names", a)
	}
}

// TestLoadMasterRejectsShortMaster proves LoadMaster refuses a master secret
// shorter than minMasterKeyBytes, rather than silently deriving every
// service's repo password from weak entropy.
func TestLoadMasterRejectsShortMaster(t *testing.T) {
	resolve := func(name string) (string, error) {
		if name != MasterSecretName {
			t.Fatalf("resolve called with unexpected name %q", name)
		}
		return "too-short", nil // 9 bytes, well under minMasterKeyBytes (32)
	}

	_, err := LoadMaster(resolve)
	if err == nil {
		t.Fatal("LoadMaster: expected an error for a sub-32-byte master, got nil")
	}
}

// TestLoadMasterAcceptsMinimumLength proves the other side of the same
// boundary: a master at or above minMasterKeyBytes is accepted and returned
// unchanged (LoadMaster treats it as an opaque byte string, not base64-decoded).
// A real deployment's master is `openssl rand -base64 32`; this test uses the
// synthetic goldenMaster, which is comfortably over the floor.
func TestLoadMasterAcceptsMinimumLength(t *testing.T) {
	resolve := func(name string) (string, error) {
		return goldenMaster, nil
	}

	got, err := LoadMaster(resolve)
	if err != nil {
		t.Fatalf("LoadMaster: unexpected error for a %d-byte master: %v", len(goldenMaster), err)
	}
	if string(got) != goldenMaster {
		t.Errorf("LoadMaster = %q, want the resolved value unchanged %q", got, goldenMaster)
	}
}
