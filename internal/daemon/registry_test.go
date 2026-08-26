// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
)

// testLogger returns a *slog.Logger writing to buf so a test can assert on
// what got logged, plus the underlying buffer.
func testLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// TestRegisterRejectsDuplicateServiceName proves register keeps the first
// container to claim a service name and rejects a second, different
// container claiming the same name: the existing registration survives
// untouched, and the rejection is logged (an operator watching the daemon's
// logs needs to see this, per the doc comment's "the caller is expected to
// alert").
func TestRegisterRejectsDuplicateServiceName(t *testing.T) {
	sched, err := schedule.New(schedule.Config{})
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	deps := orchestrator.Deps{Config: &config.Config{Schedule: "@every 1h"}}
	log, buf := testLogger()

	first := &discovery.BackupSpec{Service: "dupe", ContainerID: "container-1"}
	reg := newRegistry()
	reg.register(sched, deps, first, log, nil)

	second := &discovery.BackupSpec{Service: "dupe", ContainerID: "container-2"}
	reg.register(sched, deps, second, log, nil)

	specs := reg.specs()
	if len(specs) != 1 {
		t.Fatalf("registry has %d specs, want 1 (the duplicate must be rejected)", len(specs))
	}
	if specs[0].ContainerID != "container-1" {
		t.Fatalf("registry kept container %q, want the first-registered %q", specs[0].ContainerID, "container-1")
	}

	if !strings.Contains(buf.String(), "duplicate service name") {
		t.Errorf("expected a logged duplicate-service warning, got log output: %s", buf.String())
	}
}

// TestRegisterAllowsSameContainerReplace proves register treats a second
// call for the SAME container (not a different one) as a normal replace,
// e.g. a re-discovery after a start event re-registering the same service.
func TestRegisterAllowsSameContainerReplace(t *testing.T) {
	sched, err := schedule.New(schedule.Config{})
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	deps := orchestrator.Deps{Config: &config.Config{Schedule: "@every 1h"}}
	log, _ := testLogger()

	reg := newRegistry()
	reg.register(sched, deps, &discovery.BackupSpec{Service: "svc", ContainerID: "c1", RepoPath: "v1"}, log, nil)
	reg.register(sched, deps, &discovery.BackupSpec{Service: "svc", ContainerID: "c1", RepoPath: "v2"}, log, nil)

	specs := reg.specs()
	if len(specs) != 1 {
		t.Fatalf("registry has %d specs, want 1", len(specs))
	}
	if specs[0].RepoPath != "v2" {
		t.Fatalf("registry kept RepoPath %q, want the latest %q", specs[0].RepoPath, "v2")
	}
}

// TestUnregisterContainerReturnsServiceNameOnlyWhenRemoved proves
// unregisterContainer reports back the service name it actually dropped
// (what handleEvent needs to log a removal), and returns "" for a
// container that owns nothing, e.g. a die event for a container that was
// never opted in, or a second die/destroy event for one already removed.
func TestUnregisterContainerReturnsServiceNameOnlyWhenRemoved(t *testing.T) {
	sched, err := schedule.New(schedule.Config{})
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	deps := orchestrator.Deps{Config: &config.Config{Schedule: "@every 1h"}}
	log, _ := testLogger()

	reg := newRegistry()
	reg.register(sched, deps, &discovery.BackupSpec{Service: "svc", ContainerID: "c1"}, log, nil)

	if got := reg.unregisterContainer(sched, "unknown-container"); got != "" {
		t.Fatalf("unregisterContainer(unknown) = %q, want \"\"", got)
	}

	if got := reg.unregisterContainer(sched, "c1"); got != "svc" {
		t.Fatalf("unregisterContainer(c1) = %q, want %q", got, "svc")
	}
	if len(reg.specs()) != 0 {
		t.Fatalf("registry still has %d specs after unregister, want 0", len(reg.specs()))
	}

	// A second removal of the same (now-gone) container reports nothing:
	// idempotent, matching a die followed by a destroy event for the same
	// container.
	if got := reg.unregisterContainer(sched, "c1"); got != "" {
		t.Fatalf("second unregisterContainer(c1) = %q, want \"\"", got)
	}
}
