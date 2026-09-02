// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tagwright/beacon"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
)

// registry tracks every service Ballast currently knows how to back up: its
// resolved spec, keyed by service name, and the reverse map from container
// ID to service name that lifecycle events (which carry only an ID) need to
// look a service back up. It is the "map service->spec under a mutex" the
// daemon watch loop and the initial discovery pass both drive.
type registry struct {
	mu          sync.Mutex
	byService   map[string]*discovery.BackupSpec
	byContainer map[string]string
}

func newRegistry() *registry {
	return &registry{
		byService:   make(map[string]*discovery.BackupSpec),
		byContainer: make(map[string]string),
	}
}

// specs returns a snapshot of every currently registered spec.
func (r *registry) specs() []*discovery.BackupSpec {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]*discovery.BackupSpec, 0, len(r.byService))
	for _, spec := range r.byService {
		out = append(out, spec)
	}
	return out
}

// register adds or replaces spec's scheduled job. If a different container
// already owns spec.Service, the new spec is rejected as a duplicate: the
// existing registration is left in place, and the caller is expected to
// alert.
func (r *registry) register(sched *schedule.Scheduler, deps orchestrator.Deps, spec *discovery.BackupSpec, log *slog.Logger, notifier *beacon.Beacon) {
	r.mu.Lock()
	if existing, ok := r.byService[spec.Service]; ok && existing.ContainerID != spec.ContainerID {
		r.mu.Unlock()
		log.Error("daemon: duplicate service name, skipping",
			"service", spec.Service, "container", spec.ContainerID, "existing_container", existing.ContainerID)
		notify(notifier, beacon.LevelError, "Ballast: duplicate service name",
			fmt.Sprintf("service %q on container %s conflicts with existing container %s; the new registration was skipped",
				spec.Service, spec.ContainerID, existing.ContainerID))
		return
	}
	r.byService[spec.Service] = spec
	r.byContainer[spec.ContainerID] = spec.Service
	r.mu.Unlock()

	cronExpr := spec.Schedule
	if cronExpr == "" {
		cronExpr = deps.Config.Schedule
	}

	err := sched.Add(schedule.Job{
		Name:     spec.Service,
		Schedule: cronExpr,
		Run: func(ctx context.Context) {
			if err := orchestrator.RunBackup(ctx, spec, deps); err != nil {
				log.Error("daemon: backup run failed", "service", spec.Service, "error", err)
			}
		},
	})
	if err != nil {
		log.Error("daemon: schedule job failed", "service", spec.Service, "error", err)
	}

	// A service may also carry an optional local verify.schedule, registered as
	// its own lower-priority job. Billet drives verify fleet-wide instead, so
	// this is only ever added when the operator explicitly asked for it.
	r.scheduleVerify(sched, deps, spec, log)
}

// unregisterContainer removes whatever service containerID owns, if any,
// and cancels its scheduled job. Used on a die/destroy lifecycle event: the
// container can no longer be backed up until it starts again, at which
// point a fresh start event re-discovers and re-registers it.
//
// It returns the service name that was actually unregistered, or "" if
// containerID owned nothing (already removed, or never registered in the
// first place, e.g. a die event for a container that was never opted in).
// The caller logs on a non-empty result: a service dropping out of the
// schedule is exactly the kind of state change an operator watching the
// daemon's logs needs to see, matching register's own logging on the way
// in.
func (r *registry) unregisterContainer(sched *schedule.Scheduler, containerID string) string {
	r.mu.Lock()
	service, ok := r.byContainer[containerID]
	if ok {
		delete(r.byContainer, containerID)
		if existing, ok := r.byService[service]; ok && existing.ContainerID == containerID {
			delete(r.byService, service)
		} else {
			ok = false
		}
	}
	r.mu.Unlock()

	if !ok {
		return ""
	}
	sched.Remove(service)
	// Drop the service's verify job too, if it had one (a no-op otherwise).
	sched.Remove(verifyJobName(service))
	return service
}

// notify sends a Notification through notifier, tolerating a nil notifier
// (a no-op) since not every daemon deployment configures alerting.
func notify(notifier *beacon.Beacon, level beacon.Level, title, body string) {
	if notifier == nil {
		return
	}
	_ = notifier.Notify(context.Background(), beacon.Notification{
		Title: title,
		Body:  body,
		Level: level,
	})
}
