// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/manifest"
	"github.com/tagwright/ballast/internal/record"
	"github.com/tagwright/ballast/internal/ulid"
)

// fakeEngine is a stand-in engine for building records: only Name and Version
// are read by the record builder.
type fakeEngine struct{ version string }

func (f fakeEngine) EnsureRepo(context.Context, engine.Repo) error { return engine.ErrNotImplemented }
func (f fakeEngine) Backup(context.Context, engine.BackupRequest) (engine.BackupResult, error) {
	return engine.BackupResult{}, engine.ErrNotImplemented
}
func (f fakeEngine) Forget(context.Context, engine.Repo, engine.RetentionPolicy) error {
	return engine.ErrNotImplemented
}
func (f fakeEngine) DeleteSnapshot(context.Context, engine.Repo, string) error {
	return engine.ErrNotImplemented
}
func (f fakeEngine) Prune(context.Context, engine.Repo) error { return engine.ErrNotImplemented }
func (f fakeEngine) Check(context.Context, engine.Repo, bool) error {
	return engine.ErrNotImplemented
}
func (f fakeEngine) Snapshots(context.Context, engine.Repo) ([]engine.Snapshot, error) {
	return nil, engine.ErrNotImplemented
}
func (f fakeEngine) Restore(context.Context, engine.RestoreRequest) error {
	return engine.ErrNotImplemented
}
func (f fakeEngine) Name() string                   { return "restic" }
func (f fakeEngine) Version(context.Context) string { return f.version }

func newID(t *testing.T) string {
	t.Helper()
	id, err := ulid.New()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func baseDeps() Deps {
	return Deps{
		Engine: fakeEngine{version: "0.19.1"},
		Config: &config.Config{
			Runtime: "docker",
			Destinations: map[string]config.Destination{
				"b2":    {URL: "b2:acme-backups"},
				"local": {URL: "/repos"},
			},
		},
		HostID:  "h_3f9c1a2b7e8d4c5f",
		Version: "00.01.00b1",
	}
}

// scenarios returns representative built records covering the record's cases:
// a passing stream-only run, a failing stream run, and a passing filesystem
// run with a manifest, hooks, and a stopped workload.
func scenarios(t *testing.T) map[string]*record.Run {
	t.Helper()
	start := time.Date(2026, 9, 3, 2, 1, 0, 0, time.UTC)
	finished := start.Add(402 * time.Second)

	out := map[string]*record.Run{}

	// A: passing, stream only.
	{
		d := baseDeps()
		d.Trigger = "schedule"
		spec := &discovery.BackupSpec{
			Service: "nextcloud-db", Project: "nextcloud",
			ContainerID: "6bd94267f358", ContainerName: "nextcloud-db",
			Destination: "b2", RepoPath: "acme-backups/nextcloud-db",
		}
		o := &runOutcome{
			runID:     newID(t),
			retention: &retentionOutcome{applied: true},
			pre:       &hookOutcome{exit: 0, duration: 483 * time.Millisecond},
			streams: []streamOutcome{{
				id: "pgdump", filename: "db.sql", bytes: 52429246, exit: 0,
				duration: 17019 * time.Millisecond, produced: true,
				result: engine.BackupResult{SnapshotID: "6ddbafdc", BytesAdded: 39659922, FilesNew: 467},
			}},
		}
		out["run-pass"] = buildRunRecord(spec, d, o, nil, start, finished)
	}

	// B: failing stream run, no snapshot.
	{
		d := baseDeps()
		d.Trigger = "schedule"
		spec := &discovery.BackupSpec{
			Service: "matrix-db", Project: "matrix",
			ContainerID: "05f216c9175c", ContainerName: "matrix-db",
			Destination: "b2", RepoPath: "acme-backups/matrix-db",
		}
		streamErr := errors.New("restic: repository lock held by another process, retried 3 times")
		o := &runOutcome{
			runID: newID(t),
			streams: []streamOutcome{{
				id: "pgdump", filename: "synapse.sql", bytes: 268437777, exit: 1,
				duration: 25221 * time.Millisecond, err: streamErr,
			}},
		}
		out["run-fail"] = buildRunRecord(spec, d, o, errors.New("orchestrator: stream \"pgdump\" backup: "+streamErr.Error()), start, finished)
	}

	// C: passing filesystem run with a manifest, both hooks, and a stop.
	{
		d := baseDeps()
		d.Trigger = "manual"
		spec := &discovery.BackupSpec{
			Service: "gitea", Project: "",
			ContainerID: "aa11bb22cc33", ContainerName: "gitea",
			Destination: "local", RepoPath: "gitea",
			Paths:            []string{"/var/lib/docker/volumes/gitea_data/_data"},
			VerifyConfigured: true,
		}
		o := &runOutcome{
			runID:     newID(t),
			stopped:   true,
			retention: &retentionOutcome{applied: true},
			pre:       &hookOutcome{exit: 0, duration: 120 * time.Millisecond},
			post:      &hookOutcome{exit: 0, duration: 90 * time.Millisecond},
			fsResult: &engine.BackupResult{
				SnapshotID: "abcd1234", BytesAdded: 1048576, FilesNew: 12,
				BytesProcessed: 5242880, FilesProcessed: 340,
			},
			manifest: &manifest.Handle{
				Entries: 340, Bytes: 5242880,
				Digest:   "sha256:" + "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12",
				Location: "/var/lib/ballast/manifests/gitea/run.json",
			},
		}
		out["run-fs"] = buildRunRecord(spec, d, o, nil, start, finished)
	}

	return out
}

func TestBuildRunRecordInvariants(t *testing.T) {
	recs := scenarios(t)

	pass := recs["run-pass"]
	if pass.Exit != 0 {
		t.Errorf("run-pass exit = %d, want 0", pass.Exit)
	}
	if pass.SnapshotID == nil || *pass.SnapshotID != "6ddbafdc" {
		t.Errorf("run-pass snapshot_id = %v, want 6ddbafdc", pass.SnapshotID)
	}
	if pass.SnapshotTime == nil {
		t.Error("run-pass snapshot_time is nil, want set")
	}
	if pass.Error != nil {
		t.Errorf("run-pass error = %v, want nil", *pass.Error)
	}
	if pass.BytesAdded != 39659922 || pass.FilesNew != 467 {
		t.Errorf("run-pass bytes/files = %d/%d", pass.BytesAdded, pass.FilesNew)
	}
	if pass.BytesProcessed == nil || pass.FilesProcessed == nil {
		t.Error("run-pass processed totals should be non-null when a snapshot was produced")
	}
	if pass.RepoProperties.Backend == nil || *pass.RepoProperties.Backend != "b2" {
		t.Errorf("run-pass backend = %v, want b2", pass.RepoProperties.Backend)
	}
	if pass.RepoProperties.Encrypted == nil || !*pass.RepoProperties.Encrypted {
		t.Error("run-pass encrypted should be true (restic)")
	}
	if pass.RepoProperties.Offsite != nil || pass.RepoProperties.Immutable != nil {
		t.Error("run-pass offsite/immutable should be null (not assessed)")
	}
	if pass.Manifest != nil {
		t.Error("run-pass manifest should be null (no verify config)")
	}
	if pass.Paths == nil {
		t.Error("run-pass paths should be an empty array, not nil")
	}

	fail := recs["run-fail"]
	if fail.Exit != 1 {
		t.Errorf("run-fail exit = %d, want 1", fail.Exit)
	}
	if fail.SnapshotID != nil || fail.SnapshotTime != nil {
		t.Error("run-fail snapshot_id/time should be null")
	}
	if fail.Error == nil {
		t.Error("run-fail error should be set")
	}
	if fail.BytesProcessed != nil || fail.FilesProcessed != nil {
		t.Error("run-fail processed totals should be null (no snapshot)")
	}
	if len(fail.Streams) != 1 || fail.Streams[0].Exit != 1 || fail.Streams[0].Error == nil {
		t.Error("run-fail stream outcome not recorded correctly")
	}

	fs := recs["run-fs"]
	if fs.Exit != 0 || fs.SnapshotID == nil || *fs.SnapshotID != "abcd1234" {
		t.Errorf("run-fs exit/snapshot wrong: %d %v", fs.Exit, fs.SnapshotID)
	}
	if !fs.StoppedForBkp {
		t.Error("run-fs stopped_for_backup should be true")
	}
	if fs.Manifest == nil || fs.Manifest.Entries != 340 {
		t.Error("run-fs manifest should be set")
	}
	if fs.RepoProperties.Backend == nil || *fs.RepoProperties.Backend != "local" {
		t.Errorf("run-fs backend = %v, want local", fs.RepoProperties.Backend)
	}
	if fs.Hooks.Pre == nil || fs.Hooks.Post == nil {
		t.Error("run-fs should carry both hooks")
	}

	// Every record must round-trip through Marshal and carry the discriminator.
	for name, r := range recs {
		data, err := record.Marshal(r)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		var back map[string]any
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatalf("%s: unmarshal: %v", name, err)
		}
		if back["record"] != record.RecordType {
			t.Errorf("%s: record = %v", name, back["record"])
		}
	}

	// Optionally dump the built records for external schema validation.
	if dir := os.Getenv("BALLAST_RECORD_OUT"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, r := range recs {
			data, err := record.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name+".json"), data, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
