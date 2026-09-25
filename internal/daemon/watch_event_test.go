// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
	"github.com/tagwright/core/runtime"
	"github.com/tagwright/core/runtime/runtimetest"
)

// These cover the lifecycle-event wiring: handleEvent maps one socket event onto
// a registry mutation, and watchLoop pumps the event and error streams into it.
// This is the socket-event-to-effect layer the enforce floor exists to protect,
// so the failure knob matters here: a start event whose Inspect fails must be
// heard, not silently dropped, and a die must actually remove the service.

// watchTestCfg is a minimal config a start event can discover a service from: a
// destination, a default, and a schedule so the discovered spec registers.
func watchTestCfg() *config.Config {
	return &config.Config{
		Destinations:       map[string]config.Destination{"local": {URL: "/repos"}},
		DefaultDestination: "local",
		Schedule:           "@daily",
	}
}

func newSched(t *testing.T) *schedule.Scheduler {
	t.Helper()
	sched, err := schedule.New(schedule.Config{})
	if err != nil {
		t.Fatalf("schedule.New: %v", err)
	}
	return sched
}

// TestHandleEvent_StartRegistersDiscoveredService proves a start event inspects
// the container and registers the opted-in service it discovers.
func TestHandleEvent_StartRegistersDiscoveredService(t *testing.T) {
	rt := runtimetest.New()
	rt.Containers = []runtime.Container{{
		ID:    "c-web",
		Name:  "web",
		State: "running",
		Image: "nginx",
		Labels: map[string]string{
			"ballast.enable":  "true",
			"ballast.name":    "web",
			"ballast.volumes": "none",
		},
	}}
	cfg := watchTestCfg()
	reg := newRegistry()

	handleEvent(context.Background(), runtime.Event{Type: runtime.EventStart, ID: "c-web"},
		rt, cfg, reg, newSched(t), orchestrator.Deps{Config: cfg}, discardLog(), nil)

	if got := len(reg.specs()); got != 1 {
		t.Fatalf("a start event for an opted-in container registered %d services, want 1", got)
	}
}

// TestHandleEvent_StartInspectFailureSurfaces proves that when the post-start
// Inspect fails, the failure surfaces in the daemon log and NO service is
// registered off a container the daemon could not actually read. A silent skip
// here would leave an operator believing a just-started service is protected.
func TestHandleEvent_StartInspectFailureSurfaces(t *testing.T) {
	rt := runtimetest.New()
	rt.Faults.Inspect = errors.New("injected inspect failure")

	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	cfg := watchTestCfg()
	reg := newRegistry()

	handleEvent(context.Background(), runtime.Event{Type: runtime.EventStart, ID: "c-web"},
		rt, cfg, reg, newSched(t), orchestrator.Deps{Config: cfg}, log, nil)

	out := buf.String()
	if !contains(out, "inspect container after start event") {
		t.Fatalf("inspect failure after a start event did not surface in the log; log:\n%s", out)
	}
	if !contains(out, "injected inspect failure") {
		t.Fatalf("the start-event inspect error text was not logged; log:\n%s", out)
	}
	if got := len(reg.specs()); got != 0 {
		t.Fatalf("a container the daemon could not inspect was still registered (%d specs): the failure did not stop the registration", got)
	}
}

// TestHandleEvent_DieUnregistersService proves a die event removes the service a
// container owned, so a dead container stops being scheduled until it starts
// again.
func TestHandleEvent_DieUnregistersService(t *testing.T) {
	cfg := watchTestCfg()
	sched := newSched(t)
	reg := newRegistry()
	deps := orchestrator.Deps{Config: cfg}

	spec := &discovery.BackupSpec{
		Service:     "web",
		ContainerID: "c-web",
		Destination: "local",
		RepoPath:    "web",
		Schedule:    "@daily",
	}
	reg.register(sched, deps, spec, discardLog(), nil)
	if len(reg.specs()) != 1 {
		t.Fatalf("precondition: service not registered")
	}

	handleEvent(context.Background(), runtime.Event{Type: runtime.EventDie, ID: "c-web"},
		nil, cfg, reg, sched, deps, discardLog(), nil)

	if got := len(reg.specs()); got != 0 {
		t.Fatalf("a die event left %d services registered, want 0 (the dead container must be unregistered)", got)
	}
}

// TestHandleEvent_StopIsNotActedOn proves an EventStop (e.g. Ballast's own
// Runtime.Stop during a run) is deliberately ignored: a stopped-but-not-dead
// container is not a lifecycle change discovery reacts to, so the service stays
// registered.
func TestHandleEvent_StopIsNotActedOn(t *testing.T) {
	cfg := watchTestCfg()
	sched := newSched(t)
	reg := newRegistry()
	deps := orchestrator.Deps{Config: cfg}

	spec := &discovery.BackupSpec{
		Service:     "web",
		ContainerID: "c-web",
		Destination: "local",
		RepoPath:    "web",
		Schedule:    "@daily",
	}
	reg.register(sched, deps, spec, discardLog(), nil)

	handleEvent(context.Background(), runtime.Event{Type: runtime.EventStop, ID: "c-web"},
		nil, cfg, reg, sched, deps, discardLog(), nil)

	if got := len(reg.specs()); got != 1 {
		t.Fatalf("an EventStop changed the registry (%d specs), want the service left registered", got)
	}
}

// TestWatchLoop_DrivesStartDieAndError exercises the loop itself: it pumps a
// start (register), a die (unregister), a mid-stream error (which must reach the
// log, not be swallowed), then a clean stream close, and asserts the loop
// returns on the close.
func TestWatchLoop_DrivesStartDieAndError(t *testing.T) {
	rt := runtimetest.New()
	rt.Containers = []runtime.Container{{
		ID:    "c-web",
		Name:  "web",
		State: "running",
		Image: "nginx",
		Labels: map[string]string{
			"ballast.enable":  "true",
			"ballast.name":    "web",
			"ballast.volumes": "none",
		},
	}}
	cfg := watchTestCfg()
	sched := newSched(t)
	reg := newRegistry()
	deps := orchestrator.Deps{Config: cfg}

	var logBuf syncBuffer
	log := slog.New(slog.NewTextHandler(&logBuf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		watchLoop(ctx, rt, cfg, reg, sched, deps, log, nil)
	}()

	rt.Emit(runtime.Event{Type: runtime.EventStart, ID: "c-web"})
	waitUntil(t, 5*time.Second, func() bool { return len(reg.specs()) == 1 },
		"start event never registered the service")

	rt.Emit(runtime.Event{Type: runtime.EventDie, ID: "c-web"})
	waitUntil(t, 5*time.Second, func() bool { return len(reg.specs()) == 0 },
		"die event never unregistered the service")

	rt.Fail(errors.New("injected watch stream error"))
	waitUntil(t, 5*time.Second, func() bool { return contains(logBuf.String(), "injected watch stream error") },
		"a mid-stream watch error was swallowed instead of logged")

	rt.CloseWatch()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchLoop did not return after the event stream closed")
	}
}

// waitUntil polls cond until it holds or the timeout elapses, failing with msg.
func waitUntil(t *testing.T, timeout time.Duration, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(msg)
}

// contains is a tiny readability wrapper so the assertions above read as intent.
func contains(haystack, needle string) bool {
	return bytes.Contains([]byte(haystack), []byte(needle))
}
