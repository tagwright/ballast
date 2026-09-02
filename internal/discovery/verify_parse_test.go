// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package discovery

import (
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/core/runtime"
)

// TestParseVerifyDefaults proves an unconfigured service still reads back the
// documented defaults (files mode, 10m timeout) with Configured false, and a
// bare verify label opts in without changing the mode.
func TestParseVerifyDefaults(t *testing.T) {
	v, err := parseVerify(map[string]string{})
	if err != nil {
		t.Fatalf("parseVerify: %v", err)
	}
	if v.Configured {
		t.Errorf("Configured = true, want false for no verify labels")
	}
	if v.Mode != VerifyModeFiles {
		t.Errorf("Mode = %q, want files", v.Mode)
	}
	if v.Timeout != 10*time.Minute {
		t.Errorf("Timeout = %v, want 10m", v.Timeout)
	}

	v, err = parseVerify(map[string]string{"verify": "true"})
	if err != nil {
		t.Fatalf("parseVerify bare: %v", err)
	}
	if !v.Configured {
		t.Errorf("Configured = false, want true for bare verify label")
	}
	if v.Mode != VerifyModeFiles {
		t.Errorf("Mode = %q, want files default", v.Mode)
	}
}

// TestParseVerifyFull exercises every field and the env map.
func TestParseVerifyFull(t *testing.T) {
	v, err := parseVerify(map[string]string{
		"verify.mode":                  "stream-restore",
		"verify.probe":                 "psql -U app -tAc 'select count(*) from users'",
		"verify.expect":                "^[1-9][0-9]*$",
		"verify.timeout":               "8m",
		"verify.image":                 "postgres:16",
		"verify.schedule":              "@daily",
		"verify.data-engine":           "postgres",
		"verify.restore":               "psql -U app -d app",
		"verify.ready":                 "pg_isready -U app",
		"verify.user":                  "postgres",
		"verify.env.POSTGRES_USER":     "app",
		"verify.env.POSTGRES_DB":       "app",
		"verify.env.POSTGRES_PASSWORD": "boot",
	})
	if err != nil {
		t.Fatalf("parseVerify: %v", err)
	}
	if v.Mode != VerifyModeStreamRestore {
		t.Errorf("Mode = %q", v.Mode)
	}
	if v.Timeout != 8*time.Minute {
		t.Errorf("Timeout = %v", v.Timeout)
	}
	if v.Image != "postgres:16" || v.DataEngine != "postgres" || v.Restore != "psql -U app -d app" {
		t.Errorf("string fields not parsed: %+v", v)
	}
	if v.Ready != "pg_isready -U app" || v.User != "postgres" {
		t.Errorf("ready/user not parsed: %+v", v)
	}
	if len(v.Env) != 3 || v.Env["POSTGRES_USER"] != "app" || v.Env["POSTGRES_PASSWORD"] != "boot" {
		t.Errorf("Env = %v, want 3 entries", v.Env)
	}
}

// TestParseVerifyErrors covers the two rejections: an unknown mode and a
// stream-restore with no dump-ingest command.
func TestParseVerifyErrors(t *testing.T) {
	if _, err := parseVerify(map[string]string{"verify.mode": "bogus"}); err == nil {
		t.Error("expected error for unknown mode")
	}
	if _, err := parseVerify(map[string]string{"verify.mode": "stream-restore"}); err == nil {
		t.Error("expected error for stream-restore with no verify.restore")
	}
	if _, err := parseVerify(map[string]string{"verify.timeout": "notaduration"}); err == nil {
		t.Error("expected error for bad timeout")
	}
}

// TestDiscoverPopulatesVerifyAndImage proves Discover threads the container
// image and the parsed verify config onto the spec.
func TestDiscoverPopulatesVerifyAndImage(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	spec, _, err := Discover(runtime.Container{
		ID:    "c1",
		Name:  "app-db",
		Image: "postgres:16",
		Labels: map[string]string{
			"ballast.enable":       "true",
			"ballast.verify.mode":  "files",
			"ballast.verify.probe": "test -f /scratch/marker",
		},
	}, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if spec.Image != "postgres:16" {
		t.Errorf("spec.Image = %q, want postgres:16", spec.Image)
	}
	if !spec.Verify.Configured || spec.Verify.Mode != VerifyModeFiles {
		t.Errorf("Verify not populated: %+v", spec.Verify)
	}
	if spec.Verify.Probe != "test -f /scratch/marker" {
		t.Errorf("Verify.Probe = %q", spec.Verify.Probe)
	}
	if !spec.VerifyConfigured {
		t.Error("VerifyConfigured should mirror Verify.Configured")
	}
}
