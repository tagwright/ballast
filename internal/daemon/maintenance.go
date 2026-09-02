// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"log/slog"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
)

// Default global maintenance schedules, applied when Config leaves
// PruneSchedule/CheckSchedule empty.
const (
	defaultPruneSchedule = "@weekly"
	defaultCheckSchedule = "@monthly"
)

// scheduleMaintenance registers the two global maintenance jobs: prune and
// check. Both iterate the distinct repositories of every currently known
// service (deduplicated by destination+repo path, since several services
// can never share a repo path within one destination, but the same
// distination/path pair should still only be pruned or checked once per
// tick) and run the corresponding engine operation on each.
func scheduleMaintenance(sched *schedule.Scheduler, reg *registry, deps orchestrator.Deps, log *slog.Logger) {
	pruneSchedule := deps.Config.PruneSchedule
	if pruneSchedule == "" {
		pruneSchedule = defaultPruneSchedule
	}
	checkSchedule := deps.Config.CheckSchedule
	if checkSchedule == "" {
		checkSchedule = defaultCheckSchedule
	}

	if err := sched.Add(schedule.Job{
		Name:     "ballast:prune",
		Schedule: pruneSchedule,
		Run: func(ctx context.Context) {
			runMaintenance(ctx, "prune", reg, deps, log, deps.Engine.Prune)
		},
	}); err != nil {
		log.Error("daemon: schedule prune failed", "error", err)
	}

	if err := sched.Add(schedule.Job{
		Name:     "ballast:check",
		Schedule: checkSchedule,
		Run: func(ctx context.Context) {
			runMaintenance(ctx, "check", reg, deps, log, func(ctx context.Context, repo engine.Repo) error {
				return deps.Engine.Check(ctx, repo, false)
			})
		},
	}); err != nil {
		log.Error("daemon: schedule check failed", "error", err)
	}
}

// runMaintenance runs do against every distinct (destination, repo path)
// pair among reg's currently known services.
func runMaintenance(ctx context.Context, action string, reg *registry, deps orchestrator.Deps, log *slog.Logger, do func(ctx context.Context, repo engine.Repo) error) {
	seen := make(map[string]bool)

	for _, spec := range reg.specs() {
		key := spec.Destination + "\x00" + spec.RepoPath
		if seen[key] {
			continue
		}
		seen[key] = true

		if err := runMaintenanceOne(ctx, action, spec, deps, log, do); err != nil {
			log.Error("daemon: maintenance failed", "action", action, "service", spec.Service, "error", err)
		} else {
			log.Info("daemon: maintenance completed", "action", action, "service", spec.Service)
		}
	}
}

// runMaintenanceOne builds spec's repo and runs do against it once.
func runMaintenanceOne(ctx context.Context, action string, spec *discovery.BackupSpec, deps orchestrator.Deps, log *slog.Logger, do func(ctx context.Context, repo engine.Repo) error) error {
	repo, err := orchestrator.BuildRepo(spec, deps.Config, deps.Resolver, deps.Master)
	if err != nil {
		return err
	}
	return do(ctx, repo)
}
