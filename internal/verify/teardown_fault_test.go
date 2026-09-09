// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package verify

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/core/runtime"
)

// containerModeFixture builds a passing container-mode verify: one named-volume
// mount to restore, an engine that materializes one restored file, and exec
// results that make the tar populate, the readiness poll, and the probe all
// succeed. A test then sets a fault knob on the returned runtime and asserts the
// failure surfaces. It is the shared setup for the #680 restore-sandbox
// provisioner fault tests.
func containerModeFixture(t *testing.T) (*discovery.BackupSpec, *fakeRuntime, *fakeEngine, string) {
	t.Helper()
	stateDir := t.TempDir()
	const dataDir = "/var/lib/docker/volumes/appdata/_data"

	eng := &fakeEngine{
		snaps: []engine.Snapshot{{ID: "6ddbafdc", Time: baseTime.Add(-time.Hour)}},
		files: map[string][]byte{dataDir + "/PG_VERSION": []byte("16\n")},
	}
	c := runtime.Container{
		ID:   "abc123def456",
		Name: "app-db",
		Mounts: []runtime.Mount{
			{Type: runtime.MountVolume, Name: "appdata", Source: dataDir, Destination: "/var/lib/postgresql/data"},
		},
	}
	rt := &fakeRuntime{
		container: c,
		execs: []execResult{
			{match: "tar -x", stdout: "", exit: 0},
			{match: "isready", stdout: "", exit: 0},
			{match: "count(*)", stdout: "5", exit: 0},
		},
	}
	spec := baseSpec("app-db", discovery.VerifySpec{
		Mode:       discovery.VerifyModeContainer,
		Image:      "postgres:16",
		DataEngine: "postgres",
		Ready:      "pg_isready -U app",
		Probe:      "psql -tAc 'select count(*) from users'",
		Expect:     "re:^[1-9][0-9]*$",
		Env:        map[string]string{"POSTGRES_PASSWORD": "boot"},
	})
	return spec, rt, eng, stateDir
}

// --- teardown-remove faults: a failed removal of a scratch object must be
// recorded as a possible leak of restored data, never a silent success --------

// TestContainerMode_ScratchVolumeRemovalFailureSurfaces (#680): when the scratch
// volume holding a copy of restored production data cannot be removed, the
// verify record must report scratch_destroyed=false with the failure, so the
// leak is loud even though the probe itself passed.
func TestContainerMode_ScratchVolumeRemovalFailureSurfaces(t *testing.T) {
	spec, rt, eng, stateDir := containerModeFixture(t)
	rt.removeVolErr = errors.New("volume busy")

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))

	if v.Result != "pass" {
		t.Fatalf("probe should still pass; Result=%q reason=%q", v.Result, deref(v.Reason))
	}
	if v.ScratchDestroyed {
		t.Fatal("#680: a failed scratch-volume removal was recorded as fully destroyed (silent leak)")
	}
	if v.ScratchDestroyErr == nil || !strings.Contains(*v.ScratchDestroyErr, "volume") {
		t.Fatalf("#680: scratch_destroy_err does not name the volume removal failure: %v", deref(v.ScratchDestroyErr))
	}
}

// TestContainerMode_ScratchNetworkRemovalFailureSurfaces (#680): a failed
// removal of the isolated throwaway network is likewise recorded, not swallowed.
func TestContainerMode_ScratchNetworkRemovalFailureSurfaces(t *testing.T) {
	spec, rt, eng, stateDir := containerModeFixture(t)
	rt.removeNetErr = errors.New("network in use")

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))

	if v.ScratchDestroyed {
		t.Fatal("#680: a failed throwaway-network removal was recorded as fully destroyed (silent)")
	}
	if v.ScratchDestroyErr == nil || !strings.Contains(*v.ScratchDestroyErr, "network") {
		t.Fatalf("#680: scratch_destroy_err does not name the network removal failure: %v", deref(v.ScratchDestroyErr))
	}
}

// TestContainerMode_ThrowawayContainerRemovalFailureSurfaces (#680): a failed
// removal of the throwaway container surfaces when the container is still
// present. This exercises the idempotent teardown's "is it already gone?"
// branch: a genuine removal failure (container still present) must be surfaced,
// not mistaken for an already-removed container and swallowed.
func TestContainerMode_ThrowawayContainerRemovalFailureSurfaces(t *testing.T) {
	spec, rt, eng, stateDir := containerModeFixture(t)
	rt.removeContErr = errors.New("container still running")
	rt.inspectPresent = true // the container is genuinely still there

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))

	if v.ScratchDestroyed {
		t.Fatal("#680: a failed throwaway-container removal (container still present) was recorded as destroyed (silent leak)")
	}
	if v.ScratchDestroyErr == nil || !strings.Contains(*v.ScratchDestroyErr, "container") {
		t.Fatalf("#680: scratch_destroy_err does not name the container removal failure: %v", deref(v.ScratchDestroyErr))
	}
}

// --- create faults: a failed create of a sandbox object must surface as a
// non-pass verdict AND must not leak whatever was already created -------------

// TestContainerMode_CreateNetworkFailureSurfaces (#680): a failed isolated
// network create aborts the verify as inconclusive (never a silent pass) and
// leaks nothing, since nothing was created before it.
func TestContainerMode_CreateNetworkFailureSurfaces(t *testing.T) {
	spec, rt, eng, stateDir := containerModeFixture(t)
	rt.createNetErr = errors.New("no address space")

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))

	if v.Result != "inconclusive" {
		t.Fatalf("#680: a failed network create did not surface; Result=%q (want inconclusive)", v.Result)
	}
	if v.Reason == nil || !strings.Contains(*v.Reason, "network") {
		t.Fatalf("#680: reason does not name the network create failure: %v", deref(v.Reason))
	}
	if leaks := rt.leaked(); len(leaks) != 0 {
		t.Fatalf("#680: leaked throwaway objects after a create-network failure: %v", leaks)
	}
}

// TestContainerMode_CreateVolumeFailureTearsDownNetwork (#680): a failed scratch
// volume create aborts as inconclusive, and the isolated network created just
// before it must still be torn down. This is the partial-create case where a
// silent leak would be easiest, so it is asserted directly: leaked() must be
// empty.
func TestContainerMode_CreateVolumeFailureTearsDownNetwork(t *testing.T) {
	spec, rt, eng, stateDir := containerModeFixture(t)
	rt.createVolErr = errors.New("disk full")

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))

	if v.Result != "inconclusive" {
		t.Fatalf("#680: a failed volume create did not surface; Result=%q (want inconclusive)", v.Result)
	}
	if leaks := rt.leaked(); len(leaks) != 0 {
		t.Fatalf("#680: the network created before the volume-create failure leaked: %v", leaks)
	}
	if !v.ScratchDestroyed {
		t.Fatalf("#680: teardown of the already-created network did not complete: %v", deref(v.ScratchDestroyErr))
	}
}
