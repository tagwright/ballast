// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package discovery

import (
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/core/runtime"
)

// #600(b): an unrecognized suffix under Ballast's namespace is a typo or a
// forward-version label. Ballast neither silently ignores it (the operator
// would never learn their label did nothing) nor skips the service over it (a
// typo must never cost a working backup). Instead Discover records it on
// spec.UnknownSuffixes and the service is still discovered and backed up; the
// daemon raises the loud alert (see the daemon test). These tests prove the
// discovery half: the typo is recorded, the spec still comes back, and a fully
// valid label set records nothing.

func testConfig() *config.Config {
	return &config.Config{
		Destinations:       map[string]config.Destination{"local": {URL: "/repos"}},
		DefaultDestination: "local",
	}
}

func TestDiscover_UnknownSuffixRecordedNotSkipped(t *testing.T) {
	c := runtime.Container{
		ID:   "c1",
		Name: "/webapp",
		Labels: map[string]string{
			"ballast.enable":        "true",
			"ballast.retenton.last": "7", // typo for retention.last
		},
	}
	spec, _, err := Discover(c, testConfig())
	if err != nil {
		t.Fatalf("Discover returned an error, want the service still discovered: %v", err)
	}
	if spec == nil {
		t.Fatal("Discover returned a nil spec, want the service still backed up under its recognized labels")
	}
	if got := spec.UnknownSuffixes; len(got) != 1 || got[0] != "retenton.last" {
		t.Errorf("spec.UnknownSuffixes = %v, want [retenton.last]", got)
	}
}

func TestDiscover_UnknownSuffixRecorded_AliasPrefix(t *testing.T) {
	c := runtime.Container{
		ID:   "c1",
		Name: "/webapp",
		Labels: map[string]string{
			"tagwright.backup.enable":          "true",
			"tagwright.backup.retenton.hourly": "3", // typo under the alias prefix
		},
	}
	spec, _, err := Discover(c, testConfig())
	if err != nil {
		t.Fatalf("Discover returned an error, want the service still discovered: %v", err)
	}
	if spec == nil {
		t.Fatal("Discover returned a nil spec under the alias prefix")
	}
	if got := spec.UnknownSuffixes; len(got) != 1 || got[0] != "retenton.hourly" {
		t.Errorf("spec.UnknownSuffixes = %v, want [retenton.hourly]", got)
	}
}

func TestDiscover_ForeignNamespaceStillSilentlyIgnored(t *testing.T) {
	// A label under a wholly foreign namespace is not Ballast's and never
	// reaches the suffix check, so it is not flagged.
	c := runtime.Container{
		ID:   "c1",
		Name: "/webapp",
		Labels: map[string]string{
			"ballast.enable":     "true",
			"backup.retenton":    "oops",
			"com.example.thing":  "x",
		},
	}
	spec, _, err := Discover(c, testConfig())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if spec == nil {
		t.Fatal("nil spec")
	}
	if len(spec.UnknownSuffixes) != 0 {
		t.Errorf("spec.UnknownSuffixes = %v, want empty (foreign namespaces are silently ignored)", spec.UnknownSuffixes)
	}
}

func TestDiscover_ValidSuffixesRecordNothing(t *testing.T) {
	// A full valid label set, including a member of each open-ended family
	// (exclude.<n>, stream.<id>.<field>, verify.env.<KEY>), records no unknown
	// suffix, so the guard flags only genuine typos.
	c := runtime.Container{
		ID:   "c1",
		Name: "/db",
		Labels: map[string]string{
			"ballast.enable":              "true",
			"ballast.name":                "db",
			"ballast.repo":                "local",
			"ballast.repo.path":           "db",
			"ballast.volumes":             "none",
			"ballast.exclude.0":           "/var/cache",
			"ballast.retention.daily":     "7",
			"ballast.retention.keep-tags": "keep",
			"ballast.stream.dump.command": "pg_dumpall",
			"ballast.stream.dump.user":    "postgres",
			"ballast.verify":              "true",
			"ballast.verify.env.PGPASS":   "x",
			"ballast.notify.on-success":   "true",
			"ballast.schedule":            "@daily",
		},
	}
	cfg := &config.Config{
		Destinations:       map[string]config.Destination{"local": {URL: "/repos"}},
		DefaultDestination: "local",
		EnableExec:         true,
	}
	spec, _, err := Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover of a fully valid label set errored: %v", err)
	}
	if spec == nil {
		t.Fatal("nil spec for a valid label set")
	}
	if len(spec.UnknownSuffixes) != 0 {
		t.Errorf("spec.UnknownSuffixes = %v, want empty for an all-valid label set", spec.UnknownSuffixes)
	}
}
