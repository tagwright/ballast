// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package verify

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/record"
	"github.com/tagwright/core/runtime"
)

// filesPassFixture stands up a passing files-mode verify: a two-file manifest,
// an engine that restores the matching bytes, and the spec/container/deps a run
// needs. It is the deterministic base the flag tests vary one knob at a time
// from. baseSpec leaves the label timeout at its 1m test default.
func filesPassFixture(t *testing.T) (*discovery.BackupSpec, runtime.Container, Engine, runtime.Runtime, string) {
	t.Helper()
	stateDir := t.TempDir()
	src := filepath.Join(t.TempDir(), "data")
	files := map[string][]byte{
		filepath.Join(src, "a.txt"): []byte("alpha"),
		filepath.Join(src, "b.txt"): []byte("bravo"),
	}
	stageManifest(t, stateDir, "photos", "6ddbafdc", files)
	eng := &fakeEngine{
		snaps: []engine.Snapshot{{ID: "6ddbafdc", Time: baseTime.Add(-time.Hour)}},
		files: files,
	}
	rt := &fakeRuntime{container: runtime.Container{ID: "abc123def456", Name: "photos"}}
	spec := baseSpec("photos", discovery.VerifySpec{Mode: discovery.VerifyModeFiles})
	spec.Paths = []string{src}
	return spec, rt.container, eng, rt, stateDir
}

// TestRemoteRequestedByFlag proves the --requested-by wiring: a Deps carrying a
// remote trigger and a requester identity (exactly what the CLI sets when
// --requested-by is given) emits trigger "remote" and that identity, while the
// verdict is unaffected. The record is emitted for the external schema pass.
func TestRemoteRequestedByFlag(t *testing.T) {
	spec, c, eng, rt, stateDir := filesPassFixture(t)
	deps := baseDeps(eng, rt, stateDir)
	who := "billet/controller@evidence-host"
	deps.Trigger = "remote"
	deps.RequestedBy = &who

	v := mustRun(t, spec, c, deps)
	emit(t, "remote-requested", v)

	if v.Result != "pass" {
		t.Fatalf("Result=%q reason=%q", v.Result, deref(v.Reason))
	}
	if v.Trigger != "remote" {
		t.Errorf("trigger=%q, want remote", v.Trigger)
	}
	if deref(v.RequestedBy) != who {
		t.Errorf("requested_by=%q, want %q", deref(v.RequestedBy), who)
	}
}

// TestTimeoutOverrideFlag proves --timeout overrides the effective wall clock:
// without it, timeout_ms is the label value (1m in the fixture); with it,
// timeout_ms is the override and the spec's label is left untouched. The
// override record is emitted for the external schema pass.
func TestTimeoutOverrideFlag(t *testing.T) {
	// Baseline: no override, timeout_ms reflects the label.
	spec, c, eng, rt, stateDir := filesPassFixture(t)
	base := mustRun(t, spec, c, baseDeps(eng, rt, stateDir))
	if want := uint64(time.Minute.Milliseconds()); base.TimeoutMs != want {
		t.Fatalf("baseline timeout_ms=%d, want %d (the label)", base.TimeoutMs, want)
	}

	// Override: timeout_ms reflects the flag, not the label.
	spec2, c2, eng2, rt2, stateDir2 := filesPassFixture(t)
	override := 42 * time.Second
	deps := baseDeps(eng2, rt2, stateDir2)
	deps.TimeoutOverride = &override

	v := mustRun(t, spec2, c2, deps)
	emit(t, "remote-timeout", v)

	if want := uint64(override.Milliseconds()); v.TimeoutMs != want {
		t.Errorf("timeout_ms=%d, want %d (the override)", v.TimeoutMs, want)
	}
	if spec2.Verify.Timeout != time.Minute {
		t.Errorf("override mutated the spec label: got %v, want 1m", spec2.Verify.Timeout)
	}
}

// TestUnsetFlagsRecordUnchanged proves additivity: with neither new field set,
// the record is byte-for-byte what it is today. Two runs of the identical
// fixture differ only in the two fields that are intrinsically random per run
// (verify_id and the throwaway scratch path in environment.location); with
// those normalized the records are identical, and the trigger/requested_by
// defaults are the pre-change manual/null.
func TestUnsetFlagsRecordUnchanged(t *testing.T) {
	spec, c, eng, rt, stateDir := filesPassFixture(t)

	plain := mustRun(t, spec, c, baseDeps(eng, rt, stateDir))
	if plain.Trigger != "manual" || plain.RequestedBy != nil {
		t.Fatalf("unset-flag defaults drifted: trigger=%q requested_by=%v", plain.Trigger, plain.RequestedBy)
	}

	deps := baseDeps(eng, rt, stateDir)
	deps.TimeoutOverride = nil // the explicit unset path
	explicit := mustRun(t, spec, c, deps)

	if a, b := canonicalize(t, plain), canonicalize(t, explicit); a != b {
		t.Errorf("unset-flag record changed:\n--- plain ---\n%s\n--- explicit unset ---\n%s", a, b)
	}
}

// canonicalize marshals v after blanking the two fields that legitimately vary
// per run (verify_id and the scratch dir path), so the rest can be compared
// byte for byte.
func canonicalize(t *testing.T, v *record.Verify) string {
	t.Helper()
	cp := *v
	cp.VerifyID = ""
	cp.Environment.Location = ""
	data, err := record.MarshalVerify(&cp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(data)
}
