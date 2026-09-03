// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"strings"
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
)

func noopResolver(string) (string, error) { return "", nil }

// TestBuildRepoNoDestinationsConfigured covers the common real cause of a
// failed destination lookup: a CLI invocation that loaded no config at all (no
// --config, no BALLAST_CONFIG), so the destinations map is empty. The error
// must point at the fix, not read as a naming mistake.
func TestBuildRepoNoDestinationsConfigured(t *testing.T) {
	spec := &discovery.BackupSpec{Service: "librespeed-ts", Destination: "local"}
	cfg := &config.Config{} // no destinations: config never loaded

	_, err := BuildRepo(spec, cfg, noopResolver, nil)
	if err == nil {
		t.Fatal("BuildRepo: expected an error when no destinations are configured")
	}
	for _, want := range []string{"no destinations configured", "BALLAST_CONFIG", `"local"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

// TestBuildRepoUnknownDestinationListsConfigured covers a genuine naming
// mistake: destinations exist but the requested one is not among them. The
// error should name the miss and list what is actually configured, and must
// NOT use the empty-config phrasing.
func TestBuildRepoUnknownDestinationListsConfigured(t *testing.T) {
	spec := &discovery.BackupSpec{Service: "svc", Destination: "nope"}
	cfg := &config.Config{Destinations: map[string]config.Destination{
		"local": {URL: "/repos"},
		"r2":    {URL: "s3:example"},
	}}

	_, err := BuildRepo(spec, cfg, noopResolver, nil)
	if err == nil {
		t.Fatal("BuildRepo: expected an error for an unknown destination name")
	}
	msg := err.Error()
	for _, want := range []string{`unknown destination "nope"`, "local", "r2"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "no destinations configured") {
		t.Errorf("error %q used the empty-config phrasing for a non-empty config", msg)
	}
}
