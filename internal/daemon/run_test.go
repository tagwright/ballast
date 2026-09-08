// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tagwright/beacon"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/core/runtime"
	"github.com/tagwright/core/runtime/runtimetest"
)

// benignEngine is a no-op engine.Engine whose every method succeeds. The
// failure this test injects is at the runtime layer (the stream dump exec), so
// the engine must not be what fails: it is here only so RunBackup reaches the
// stream step where the injected fault lands.
type benignEngine struct{}

func (benignEngine) EnsureRepo(context.Context, engine.Repo) error { return nil }
func (benignEngine) Backup(context.Context, engine.BackupRequest) (engine.BackupResult, error) {
	return engine.BackupResult{}, nil
}
func (benignEngine) Forget(context.Context, engine.Repo, engine.RetentionPolicy) error { return nil }
func (benignEngine) DeleteSnapshot(context.Context, engine.Repo, string) error         { return nil }
func (benignEngine) Prune(context.Context, engine.Repo) error                          { return nil }
func (benignEngine) Check(context.Context, engine.Repo, bool) error                    { return nil }
func (benignEngine) Snapshots(context.Context, engine.Repo) ([]engine.Snapshot, error) {
	return nil, nil
}
func (benignEngine) Restore(context.Context, engine.RestoreRequest) error { return nil }
func (benignEngine) Name() string                                        { return "benign" }

// TestRun_InjectedDumpFailureSurfaces is the exemplar Level 2 wiring test for
// the Testing Standard (#549/#550): it drives ballast through its real
// production entry seam, run(Deps), with the canonical fake runtime, injects a
// failure on a runtime operation (the stream dump exec), and asserts that
// failure SURFACES in the run record as a non-zero exit with an error, never a
// silent pass. Without the injected fault (Faults.Exec) this test would prove
// only the happy path, which the suite audit found is where the fake tier
// misses every real bug; the standard forbids a double with no error knob for
// exactly that reason.
func TestRun_InjectedDumpFailureSurfaces(t *testing.T) {
	stateDir := t.TempDir()

	rt := runtimetest.New()
	rt.Containers = []runtime.Container{{
		ID:    "c-abc123",
		Name:  "kimai-db",
		State: "running",
		Image: "mysql:8.4",
		Labels: map[string]string{
			"ballast.enable":              "true",
			"ballast.name":                "kimai",
			"ballast.volumes":             "none",
			"ballast.stream.dump.command": "mysqldump --databases kimai",
			"ballast.schedule":            "@every 1ms",
		},
	}}
	// The injected failure: the dump exec fails on demand. This is the knob
	// the standard requires a wiring test to trip.
	injected := errors.New("injected dump failure")
	rt.Faults.Exec = injected

	notifier, err := beacon.New(beacon.Config{}, nil)
	if err != nil {
		t.Fatalf("beacon.New: %v", err)
	}

	splayOff := false
	cfg := &config.Config{
		Runtime:            "docker",
		Destinations:       map[string]config.Destination{"local": {URL: "/repos"}},
		DefaultDestination: "local",
		Retention:          "daily=7",
		EnableExec:         true,
		Concurrency:        1,
		Splay:              &splayOff,
		StateDir:           stateDir,
	}

	deps := Deps{
		Runtime:  rt,
		Engine:   benignEngine{},
		Notifier: notifier,
		Clock:    rt.Clock.Now, // fake clock drives the scheduler deterministically
		Config:   cfg,
		Version:  "test",
		Master:   []byte("test-master-key-not-a-real-secret"),
		HostID:   "h_0000000000000000",
		StateDir: stateDir,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- run(ctx, deps) }()

	recPath := waitForRunRecord(t, filepath.Join(stateDir, "runs", "kimai"), 10*time.Second)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after ctx cancel")
	}

	var rec struct {
		Exit  int     `json:"exit"`
		Error *string `json:"error"`
	}
	b, err := os.ReadFile(recPath)
	if err != nil {
		t.Fatalf("read run record: %v", err)
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatalf("unmarshal run record: %v", err)
	}

	if rec.Exit == 0 {
		t.Fatalf("injected dump failure did not surface: run record has exit 0 (a silent success)")
	}
	if rec.Error == nil || *rec.Error == "" {
		t.Fatalf("injected dump failure did not surface: run record has no error string")
	}
}

// waitForRunRecord polls dir until a run-record JSON file appears, returning its
// path, or fails the test after timeout.
func waitForRunRecord(t *testing.T, dir string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".json" {
				return filepath.Join(dir, e.Name())
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no run record appeared under %s within %v", dir, timeout)
	return ""
}
