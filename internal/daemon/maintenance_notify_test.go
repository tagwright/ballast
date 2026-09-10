// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/tagwright/courier"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/orchestrator"
)

// #599: a scheduled prune or integrity check runs in the scheduler goroutine
// with no exit code, so beacon is the ONLY operator-facing surface for its
// failure. maintenance.go used to route those failures to log.Error alone,
// which meant an operator watching beacon alerts never heard that a scheduled
// prune failed or that a scheduled integrity check (repository-corruption
// detection) failed. That is the #598/#655 silent-failure class, and the
// charter says a failure must be loud. These tests lock in that each
// maintenance failure branch now also reaches the notifier at Error level.

// maintCapture records every notification a beacon channel delivers to it, so
// a test can assert a maintenance failure reached the operator-facing notifier.
type maintCapture struct {
	mu   sync.Mutex
	sent []courier.Notification
}

func (c *maintCapture) Name() string { return "ballast-maint-capture" }

func (c *maintCapture) Send(_ context.Context, n courier.Notification) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, n)
	return nil
}

func (c *maintCapture) notifications() []courier.Notification {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]courier.Notification(nil), c.sent...)
}

// maintCaptures lets the globally registered backend factory hand the right
// capture instance back to the test that created it, keyed by an id passed
// through the channel Settings. RegisterBackend is process-global and panics on
// a double register, so the type is registered exactly once from init.
var (
	maintCaptureMu sync.Mutex
	maintCaptures  = map[string]*maintCapture{}
)

func init() {
	courier.RegisterBackend("ballast-maint-capture", func(settings map[string]string, _ courier.SecretResolver) (courier.Backend, error) {
		maintCaptureMu.Lock()
		defer maintCaptureMu.Unlock()
		c := maintCaptures[settings["id"]]
		if c == nil {
			c = &maintCapture{}
			maintCaptures[settings["id"]] = c
		}
		return c, nil
	})
}

// captureNotifier builds a Beacon whose single channel is a capture backend
// registered under this test's unique name, and returns it with the capture.
func captureNotifier(t *testing.T) (*courier.Beacon, *maintCapture) {
	t.Helper()
	id := t.Name()
	c := &maintCapture{}
	maintCaptureMu.Lock()
	maintCaptures[id] = c
	maintCaptureMu.Unlock()

	b, err := courier.New(courier.Config{
		Channels: []courier.ChannelConfig{{
			Type:     "ballast-maint-capture",
			MinLevel: courier.LevelInfo,
			Settings: map[string]string{"id": id},
		}},
	}, nil)
	if err != nil {
		t.Fatalf("courier.New: %v", err)
	}
	return b, c
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func seedReg(specs ...*discovery.BackupSpec) *registry {
	reg := newRegistry()
	for _, s := range specs {
		reg.byService[s.Service] = s
	}
	return reg
}

// TestScheduledPruneFailure_SurfacesToNotifier proves a scheduled prune whose
// engine operation fails reaches the operator-facing notifier at Error level,
// not only the daemon log.
func TestScheduledPruneFailure_SurfacesToNotifier(t *testing.T) {
	notifier, cap := captureNotifier(t)

	cfg := &config.Config{
		Destinations: map[string]config.Destination{"local": {URL: "/repos"}},
	}
	deps := orchestrator.Deps{
		Config:   cfg,
		Master:   []byte("test-master-key-not-a-real-secret"),
		Notifier: notifier,
	}
	reg := seedReg(&discovery.BackupSpec{Service: "svc", Destination: "local", RepoPath: "svc"})

	// BuildRepo succeeds (a valid destination, master-derived password, no
	// secret env), so the failure lands on the engine op itself.
	failPrune := func(context.Context, engine.Repo) error { return errors.New("prune boom") }
	runMaintenance(context.Background(), "prune", reg, deps, discardLog(), failPrune)

	notes := cap.notifications()
	if len(notes) != 1 {
		t.Fatalf("a failed scheduled prune fired %d notifications, want exactly 1 (it must reach the operator, not only the log)", len(notes))
	}
	n := notes[0]
	if n.Level != courier.LevelError {
		t.Errorf("prune-failure notification level = %v, want LevelError", n.Level)
	}
	if !strings.Contains(n.Body, "prune boom") {
		t.Errorf("prune-failure notification body = %q, want it to carry the engine error", n.Body)
	}
	if !strings.Contains(strings.ToLower(n.Body+n.Title), "prune") {
		t.Errorf("prune-failure notification (%q / %q) should name the failed action", n.Title, n.Body)
	}
}

// TestScheduledCheckFailure_SurfacesToNotifier proves a scheduled integrity
// check that cannot even build its repo (the BuildRepo failure branch) reaches
// the notifier at Error level. This is the corruption-detection job, so a
// silent failure here is the sharpest #599 case.
func TestScheduledCheckFailure_SurfacesToNotifier(t *testing.T) {
	notifier, cap := captureNotifier(t)

	// A configured destination exists, but the spec names a different one, so
	// BuildRepo returns an "unknown destination" error before any engine call.
	cfg := &config.Config{
		Destinations: map[string]config.Destination{"local": {URL: "/repos"}},
	}
	deps := orchestrator.Deps{
		Config:   cfg,
		Master:   []byte("test-master-key-not-a-real-secret"),
		Notifier: notifier,
	}
	spec := &discovery.BackupSpec{Service: "svc", Destination: "gone", RepoPath: "svc"}

	runCheckOne(context.Background(), spec, deps, discardLog())

	notes := cap.notifications()
	if len(notes) != 1 {
		t.Fatalf("a failed scheduled check fired %d notifications, want exactly 1", len(notes))
	}
	n := notes[0]
	if n.Level != courier.LevelError {
		t.Errorf("check-failure notification level = %v, want LevelError", n.Level)
	}
	if !strings.Contains(strings.ToLower(n.Title+n.Body), "check") {
		t.Errorf("check-failure notification (%q / %q) should name the check action", n.Title, n.Body)
	}
}
