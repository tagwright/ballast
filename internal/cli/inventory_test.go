// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/core/runtime"
)

// TestBuildInventoryReflectsDiscoveredService proves the inventory record
// carries a discovered service's own resolved view: its enabled flag, the
// runtime locator, the repository id, and the verify_configured/probe_declared
// pair, all derived from the same discovery pass the daemon drives rather than
// a re-read of raw labels. It then confirms the emitted document is valid JSON
// with the documented ballast.inventory.v1 fields.
func TestBuildInventoryReflectsDiscoveredService(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load(\"\"): %v", err)
	}

	c := runtime.Container{
		ID:      "c1abc",
		Name:    "nextcloud-app-1",
		Service: "app",
		Project: "nextcloud",
		Image:   "nextcloud:latest",
		Labels: map[string]string{
			"ballast.enable":        "true",
			"ballast.name":          "nextcloud",
			"ballast.repo":          "offsite",
			"ballast.schedule":      "0 3 * * *",
			"ballast.verify.mode":   "container",
			"ballast.verify.probe":  "curl -fsS localhost/status.php",
		},
	}

	spec, _, err := discovery.Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if spec == nil {
		t.Fatal("Discover returned a nil spec for an enabled container")
	}

	inv := buildInventory([]*discovery.BackupSpec{spec}, "h_deadbeef", "docker", time.Now())

	if inv.Record != "ballast.inventory.v1" {
		t.Errorf("inv.Record = %q, want %q", inv.Record, "ballast.inventory.v1")
	}
	if inv.HostID != "h_deadbeef" {
		t.Errorf("inv.HostID = %q, want %q", inv.HostID, "h_deadbeef")
	}
	if len(inv.Services) != 1 {
		t.Fatalf("len(inv.Services) = %d, want 1", len(inv.Services))
	}

	s := inv.Services[0]
	if s.Name != "nextcloud" {
		t.Errorf("service Name = %q, want %q (ballast.name wins over the compose service)", s.Name, "nextcloud")
	}
	if !s.Enabled {
		t.Error("service Enabled = false, want true for a discovered service")
	}
	if s.Runtime != "docker" {
		t.Errorf("service Runtime = %q, want %q", s.Runtime, "docker")
	}
	if s.RepoID != "offsite:nextcloud" {
		t.Errorf("service RepoID = %q, want %q (destination:repo-path, repo-path defaulting to the resolved name)", s.RepoID, "offsite:nextcloud")
	}
	if !s.VerifyConfigured {
		t.Error("service VerifyConfigured = false, want true (verify.* labels present)")
	}
	if !s.ProbeDeclared {
		t.Error("service ProbeDeclared = false, want true (verify.probe present)")
	}
	if s.BackupSchedule == nil || *s.BackupSchedule != "0 3 * * *" {
		t.Errorf("service BackupSchedule = %v, want %q", s.BackupSchedule, "0 3 * * *")
	}

	if s.RuntimeRef["container_id"] != "c1abc" {
		t.Errorf("runtime_ref container_id = %q, want %q", s.RuntimeRef["container_id"], "c1abc")
	}
	if s.RuntimeRef["container_name"] != "nextcloud-app-1" {
		t.Errorf("runtime_ref container_name = %q, want %q", s.RuntimeRef["container_name"], "nextcloud-app-1")
	}
	if s.RuntimeRef["compose_project"] != "nextcloud" {
		t.Errorf("runtime_ref compose_project = %q, want %q", s.RuntimeRef["compose_project"], "nextcloud")
	}

	// The emitted document must be valid JSON carrying the documented fields.
	data, err := json.MarshalIndent(inv, "", "  ")
	if err != nil {
		t.Fatalf("marshal inventory: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("emitted inventory is not valid JSON: %v", err)
	}
	for _, key := range []string{"record", "host_id", "generated_at", "services"} {
		if _, ok := round[key]; !ok {
			t.Errorf("emitted inventory is missing documented field %q", key)
		}
	}

	// generated_at is RFC3339 UTC with a literal Z, like the run/verify records.
	genAt, _ := round["generated_at"].(string)
	if _, err := time.Parse(time.RFC3339, genAt); err != nil {
		t.Errorf("generated_at %q does not parse as RFC3339: %v", genAt, err)
	}
	if n := len(genAt); n == 0 || genAt[n-1] != 'Z' {
		t.Errorf("generated_at = %q, want a literal trailing Z (UTC)", genAt)
	}
}

// TestBuildInventoryScheduleNullWhenUnset proves a service with no
// ballast.schedule label reports backup_schedule as JSON null (the global
// default applies), never an empty string, so a reader cannot confuse "unset"
// with a schedule of "".
func TestBuildInventoryScheduleNullWhenUnset(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load(\"\"): %v", err)
	}

	c := runtime.Container{
		ID:   "c2",
		Name: "plain",
		Labels: map[string]string{
			"ballast.enable": "true",
		},
	}

	spec, _, err := discovery.Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	inv := buildInventory([]*discovery.BackupSpec{spec}, "h_abcd", "docker", time.Now())
	if len(inv.Services) != 1 {
		t.Fatalf("len(inv.Services) = %d, want 1", len(inv.Services))
	}
	s := inv.Services[0]
	if s.BackupSchedule != nil {
		t.Errorf("BackupSchedule = %q, want nil (null) for an unset schedule", *s.BackupSchedule)
	}
	if s.VerifyConfigured {
		t.Error("VerifyConfigured = true, want false for a service with no verify.* labels")
	}
	if s.ProbeDeclared {
		t.Error("ProbeDeclared = true, want false for a service with no verify.probe")
	}

	data, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round struct {
		Services []struct {
			BackupSchedule *string `json:"backup_schedule"`
		} `json:"services"`
	}
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.Services[0].BackupSchedule != nil {
		t.Errorf("backup_schedule serialized to %q, want JSON null", *round.Services[0].BackupSchedule)
	}
}
