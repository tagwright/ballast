// SPDX-License-Identifier: GPL-3.0-or-later

package verify

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/manifest"
	"github.com/tagwright/ballast/internal/record"
	"github.com/tagwright/core/runtime"
)

var baseTime = time.Date(2026, 9, 3, 3, 0, 0, 0, time.UTC)

// emit writes the record to VERIFY_RECORD_OUT (when set) under name.json so an
// external jsonschema pass can validate it against the frozen contract.
func emit(t *testing.T, name string, v *record.Verify) {
	t.Helper()
	out := os.Getenv("VERIFY_RECORD_OUT")
	if out == "" {
		return
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir out: %v", err)
	}
	data, err := record.MarshalVerify(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(out, name+".json"), data, 0o644); err != nil {
		t.Fatalf("write record: %v", err)
	}
}

func baseSpec(service string, v discovery.VerifySpec) *discovery.BackupSpec {
	if v.Timeout == 0 {
		v.Timeout = time.Minute
	}
	v.Configured = true
	return &discovery.BackupSpec{
		Service:       service,
		Project:       "app",
		ContainerID:   "abc123def456",
		ContainerName: service,
		Image:         "postgres:16",
		Destination:   "b2",
		RepoPath:      "acme-backups/" + service,
		Verify:        v,
	}
}

func baseDeps(eng Engine, rt runtime.Runtime, stateDir string) Deps {
	return Deps{
		Runtime:     rt,
		Engine:      eng,
		Repo:        engine.Repo{URL: "b2:acme-backups"},
		StateDir:    stateDir,
		HostID:      "h_3f9c1a2b7e8d4c5f",
		Version:     "00.01.00b1",
		RuntimeName: "docker",
		Trigger:     "manual",
		NamePrefix:  "ballast-verify-itest",
		Now:         fixedClock(baseTime, 700*time.Millisecond),
	}
}

func mustRun(t *testing.T, spec *discovery.BackupSpec, c runtime.Container, d Deps) *record.Verify {
	t.Helper()
	v, err := Run(context.Background(), spec, c, "latest", d)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return v
}

// --- files mode -----------------------------------------------------------

func stageManifest(t *testing.T, stateDir, service, snapID string, files map[string][]byte) {
	t.Helper()
	manLoc := filepath.Join(stateDir, "manifests", service, "run1.json")
	var roots []string
	seen := map[string]bool{}
	for abs := range files {
		dir := filepath.Dir(abs)
		if !seen[dir] {
			seen[dir] = true
			roots = append(roots, dir)
		}
	}
	// Materialize the source files so manifest.Build can hash them.
	for abs, content := range files {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h, err := manifest.Build(roots, manLoc)
	if err != nil {
		t.Fatalf("manifest.Build: %v", err)
	}
	snap := snapID
	run := &record.Run{
		Record:     record.RecordType,
		RunID:      "run1",
		Service:    service,
		SnapshotID: &snap,
		Manifest:   &record.Manifest{Entries: h.Entries, Bytes: h.Bytes, Digest: h.Digest, Location: h.Location},
	}
	if _, err := record.Write(stateDir, run); err != nil {
		t.Fatalf("record.Write: %v", err)
	}
}

func TestFilesModePass(t *testing.T) {
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

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))
	emit(t, "files-pass", v)

	if v.Result != "pass" {
		t.Fatalf("Result = %q reason=%v", v.Result, deref(v.Reason))
	}
	if v.ManifestCompare == nil || v.ManifestCompare.Missing != 0 || v.ManifestCompare.Mismatched != 0 {
		t.Errorf("manifest_compare = %+v", v.ManifestCompare)
	}
	if v.Assertion != "manifest" || v.ReasonCode != nil {
		t.Errorf("assertion=%q reason_code=%v", v.Assertion, deref(v.ReasonCode))
	}
}

func TestFilesModeManifestMismatch(t *testing.T) {
	stateDir := t.TempDir()
	src := filepath.Join(t.TempDir(), "data")
	files := map[string][]byte{filepath.Join(src, "a.txt"): []byte("alpha")}
	stageManifest(t, stateDir, "photos", "6ddbafdc", files)

	// The engine restores different bytes than the manifest recorded.
	eng := &fakeEngine{
		snaps: []engine.Snapshot{{ID: "6ddbafdc", Time: baseTime.Add(-time.Hour)}},
		files: map[string][]byte{filepath.Join(src, "a.txt"): []byte("CORRUPT")},
	}
	rt := &fakeRuntime{container: runtime.Container{ID: "abc123def456", Name: "photos"}}
	spec := baseSpec("photos", discovery.VerifySpec{Mode: discovery.VerifyModeFiles})
	spec.Paths = []string{src}

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))
	emit(t, "files-fail", v)

	if v.Result != "fail" || deref(v.ReasonCode) != "manifest_mismatch" {
		t.Fatalf("Result=%q code=%q", v.Result, deref(v.ReasonCode))
	}
}

// --- stream-restore mode --------------------------------------------------

func streamSpec(service string) *discovery.BackupSpec {
	return baseSpec(service, discovery.VerifySpec{
		Mode:       discovery.VerifyModeStreamRestore,
		Image:      "postgres:16",
		DataEngine: "postgres",
		Restore:    "psql --set ON_ERROR_STOP=1 -U app -d app",
		Ready:      "pg_isready -U app",
		Probe:      "psql -tAc 'select count(*) from users'",
		Expect:     "^[1-9][0-9]*$",
		Env:        map[string]string{"POSTGRES_PASSWORD": "boot"},
	})
}

func streamEngine() *fakeEngine {
	return &fakeEngine{
		snaps: []engine.Snapshot{{ID: "6ddbafdc", Time: baseTime.Add(-time.Hour)}},
		files: map[string][]byte{"/db.sql": []byte("-- dump\nINSERT ...\n")},
	}
}

func streamExecs(probeOut string, probeExit int) []execResult {
	return []execResult{
		{match: "isready", stdout: "", exit: 0},
		{match: "ON_ERROR_STOP", stdout: "", exit: 0},
		{match: "count(*)", stdout: probeOut, exit: probeExit},
	}
}

func TestStreamRestorePass(t *testing.T) {
	stateDir := t.TempDir()
	rt := &fakeRuntime{
		container: runtime.Container{ID: "abc123def456", Name: "app-db"},
		execs:     streamExecs("417", 0),
	}
	v := mustRun(t, streamSpec("app-db"), rt.container, baseDeps(streamEngine(), rt, stateDir))
	emit(t, "stream-pass", v)

	if v.Result != "pass" {
		t.Fatalf("Result=%q reason=%q", v.Result, deref(v.Reason))
	}
	if v.Assertion != "probe_expect" || v.Checked["rows"] != 417 {
		t.Errorf("assertion=%q checked=%v", v.Assertion, v.Checked)
	}
	if !v.Environment.NetworkIsolated || v.Environment.Kind != "throwaway-container" {
		t.Errorf("environment=%+v", v.Environment)
	}
	if !v.ScratchDestroyed {
		t.Errorf("scratch not destroyed: %v", deref(v.ScratchDestroyErr))
	}
	if leaks := rt.leaked(); len(leaks) != 0 {
		t.Errorf("leaked throwaway objects: %v", leaks)
	}
}

func TestStreamRestoreExpectMismatch(t *testing.T) {
	stateDir := t.TempDir()
	rt := &fakeRuntime{
		container: runtime.Container{ID: "abc123def456", Name: "app-db"},
		execs:     streamExecs("0", 0),
	}
	v := mustRun(t, streamSpec("app-db"), rt.container, baseDeps(streamEngine(), rt, stateDir))
	emit(t, "stream-fail", v)

	if v.Result != "fail" || deref(v.ReasonCode) != "expect_mismatch" {
		t.Fatalf("Result=%q code=%q", v.Result, deref(v.ReasonCode))
	}
	if leaks := rt.leaked(); len(leaks) != 0 {
		t.Errorf("leaked: %v", leaks)
	}
}

func TestStreamRestoreImageUnavailable(t *testing.T) {
	stateDir := t.TempDir()
	rt := &fakeRuntime{
		container: runtime.Container{ID: "abc123def456", Name: "app-db"},
		execs:     streamExecs("417", 0),
		pullErr:   errImagePull{},
	}
	v := mustRun(t, streamSpec("app-db"), rt.container, baseDeps(streamEngine(), rt, stateDir))
	emit(t, "stream-inconclusive", v)

	if v.Result != "inconclusive" || deref(v.ReasonCode) != "image_unavailable" {
		t.Fatalf("Result=%q code=%q", v.Result, deref(v.ReasonCode))
	}
	// Nothing was restored or checked before the pull failed.
	if v.RestoreDurationMs != 0 || len(v.Checked) != 0 {
		t.Errorf("restore_ms=%d checked=%v, want both empty", v.RestoreDurationMs, v.Checked)
	}
	if leaks := rt.leaked(); len(leaks) != 0 {
		t.Errorf("leaked: %v", leaks)
	}
}

type errImagePull struct{}

func (errImagePull) Error() string { return "registry mirror unreachable" }

// --- container mode -------------------------------------------------------

func TestContainerModePass(t *testing.T) {
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
		Expect:     "^[1-9][0-9]*$",
		Env:        map[string]string{"POSTGRES_PASSWORD": "boot"},
	})

	v := mustRun(t, spec, c, baseDeps(eng, rt, stateDir))
	emit(t, "container-pass", v)

	if v.Result != "pass" {
		t.Fatalf("Result=%q reason=%q", v.Result, deref(v.Reason))
	}
	if v.Restored.Kind != "volumes" || len(v.Restored.Items) != 1 {
		t.Errorf("restored=%+v", v.Restored)
	}
	if v.Checked["rows"] != 5 {
		t.Errorf("checked=%v", v.Checked)
	}
	if leaks := rt.leaked(); len(leaks) != 0 {
		t.Errorf("leaked throwaway objects: %v", leaks)
	}
}

// --- inconclusive: snapshot missing ---------------------------------------

func TestSnapshotMissing(t *testing.T) {
	stateDir := t.TempDir()
	eng := &fakeEngine{snaps: nil}
	rt := &fakeRuntime{container: runtime.Container{ID: "abc123def456", Name: "photos"}}
	spec := baseSpec("photos", discovery.VerifySpec{Mode: discovery.VerifyModeFiles})
	spec.Paths = []string{"/data"}

	v := mustRun(t, spec, rt.container, baseDeps(eng, rt, stateDir))
	if v.Result != "inconclusive" || deref(v.ReasonCode) != "snapshot_missing" {
		t.Fatalf("Result=%q code=%q", v.Result, deref(v.ReasonCode))
	}
	if v.SnapshotID != nil {
		t.Errorf("snapshot_id should be null, got %q", *v.SnapshotID)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
