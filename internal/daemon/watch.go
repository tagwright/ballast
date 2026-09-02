// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tagwright/beacon"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
	"github.com/tagwright/core/runtime"
)

// discoverAll lists every container the runtime knows about and registers a
// scheduled job for each one that opts in. It is the daemon's initial
// discovery pass, run once before the watch loop and the scheduler start.
func discoverAll(ctx context.Context, rt runtime.Runtime, cfg *config.Config, reg *registry, sched *schedule.Scheduler, deps orchestrator.Deps, log *slog.Logger, notifier *beacon.Beacon) error {
	containers, err := rt.List(ctx)
	if err != nil {
		return fmt.Errorf("daemon: list containers: %w", err)
	}

	for _, c := range containers {
		discoverOne(c, cfg, reg, sched, deps, log, notifier)
	}
	return nil
}

// discoverOne runs discovery against a single container and, if it opts in,
// registers its scheduled job. It logs discovery warnings, and alerts (in
// addition to logging) on a discovery error, since a skipped service with a
// validation problem is exactly the kind of thing an operator wants to hear
// about.
func discoverOne(c runtime.Container, cfg *config.Config, reg *registry, sched *schedule.Scheduler, deps orchestrator.Deps, log *slog.Logger, notifier *beacon.Beacon) {
	spec, warnings, err := discovery.Discover(c, cfg)
	for _, w := range warnings {
		log.Warn("daemon: discovery warning", "warning", w)
	}
	if err != nil {
		log.Error("daemon: discovery failed", "container", c.Name, "error", err)
		notify(notifier, beacon.LevelWarning, "Ballast: discovery error",
			fmt.Sprintf("container %s: %v", c.Name, err))
		return
	}
	if spec == nil {
		return // not opted in; the normal, silent case
	}

	reg.register(sched, deps, spec, log, notifier)
}

// watchLoop subscribes to the runtime's lifecycle events and keeps reg (and
// the scheduler) in sync: a start event re-discovers the container and
// adds or updates its job, a die or destroy event removes it. It returns
// when ctx is cancelled or the event stream ends.
func watchLoop(ctx context.Context, rt runtime.Runtime, cfg *config.Config, reg *registry, sched *schedule.Scheduler, deps orchestrator.Deps, log *slog.Logger, notifier *beacon.Beacon) {
	events, errs := rt.Watch(ctx)

	for {
		select {
		case <-ctx.Done():
			return

		case ev, ok := <-events:
			if !ok {
				return
			}
			handleEvent(ctx, ev, rt, cfg, reg, sched, deps, log, notifier)

		case err, ok := <-errs:
			if !ok {
				continue
			}
			if err != nil {
				log.Error("daemon: watch error", "error", err)
			}
		}
	}
}

// handleEvent applies one runtime.Event to reg and sched.
func handleEvent(ctx context.Context, ev runtime.Event, rt runtime.Runtime, cfg *config.Config, reg *registry, sched *schedule.Scheduler, deps orchestrator.Deps, log *slog.Logger, notifier *beacon.Beacon) {
	switch ev.Type {
	case runtime.EventStart:
		c, err := rt.Inspect(ctx, ev.ID)
		if err != nil {
			log.Error("daemon: inspect container after start event", "container", ev.ID, "error", err)
			return
		}
		discoverOne(c, cfg, reg, sched, deps, log, notifier)

	case runtime.EventDie, runtime.EventDestroy:
		if service := reg.unregisterContainer(sched, ev.ID); service != "" {
			log.Info("daemon: service unregistered", "service", service, "container", ev.ID, "event", string(ev.Type))
		}

	default:
		// EventStop and anything else the runtime might one day report are
		// not acted on: a stopped-but-not-dead container (e.g. Ballast's own
		// Runtime.Stop during a run) is not a lifecycle change discovery
		// needs to react to.
	}
}
