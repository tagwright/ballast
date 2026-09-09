// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/core/runtime/runtimetest"
)

// okEngine is a backup engine whose relevant ops succeed, so a test can drive
// the backup to success and reach the post-backup container restart.
type okEngine struct{}

func (okEngine) EnsureRepo(context.Context, engine.Repo) error { return nil }
func (okEngine) Backup(context.Context, engine.BackupRequest) (engine.BackupResult, error) {
	return engine.BackupResult{SnapshotID: "snap-ok", BytesAdded: 1, FilesNew: 1}, nil
}
func (okEngine) Forget(context.Context, engine.Repo, engine.RetentionPolicy) error         { return nil }
func (okEngine) DeleteSnapshot(context.Context, engine.Repo, string) error                 { return nil }
func (okEngine) Prune(context.Context, engine.Repo) error                                  { return nil }
func (okEngine) Check(context.Context, engine.Repo, bool) error                            { return nil }
func (okEngine) Snapshots(context.Context, engine.Repo) ([]engine.Snapshot, error)         { return nil, nil }
func (okEngine) Restore(context.Context, engine.RestoreRequest) error                      { return nil }
func (okEngine) Name() string                                                              { return "restic" }
func (okEngine) Version(context.Context) string                                            { return "0.19.1" }

// TestRunBackupSteps_FailedRestartSurfaces is the #598 regression: when a
// container ballast stopped for a consistent backup fails to restart, that
// failure must SURFACE in the run outcome (a non-nil error, which becomes a
// non-zero exit + error in the run record), not be swallowed as a silent
// success while the workload stays down.
func TestRunBackupSteps_FailedRestartSurfaces(t *testing.T) {
	rt := runtimetest.New()
	rt.Faults.Start = errors.New("simulated restart failure")

	d := Deps{
		Runtime: rt,
		Engine:  okEngine{},
		Config:  &config.Config{Runtime: "docker", Retention: "daily=7"},
	}
	spec := &discovery.BackupSpec{Service: "db", ContainerID: "c1", Stop: true, Paths: []string{"/data"}}
	out := &runOutcome{}

	err := runBackupSteps(context.Background(), spec, engine.Repo{URL: "/repos"}, d, slog.New(slog.NewTextHandler(io.Discard, nil)), out)
	if err == nil {
		t.Fatal("#598: a failed container restart after ballast.stop did not surface (silent success)")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Fatalf("#598: surfaced error does not name the restart failure: %v", err)
	}
	if !out.stopped {
		t.Fatal("out.stopped should be true (the container was stopped for backup)")
	}
}
