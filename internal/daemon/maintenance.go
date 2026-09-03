// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"log/slog"

	"github.com/tagwright/ballast/internal/check"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/record"
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
			runCheckMaintenance(ctx, reg, deps, log)
		},
	}); err != nil {
		log.Error("daemon: schedule check failed", "error", err)
	}
}

// runCheckMaintenance runs an integrity check against every distinct
// (destination, repo path) pair among reg's currently known services and writes
// a ballast.check.v1 record for each, closing the gap where a scheduled check
// left only a log line and no evidence. The scheduled check is metadata-only
// (readData is false, unchanged), so every record it writes carries method
// "metadata": the cheap structural check, NOT a --read-data pass, and NOT a
// restore test. The check itself always runs; only the evidence write is gated
// on a state dir and a stable host identity being available.
func runCheckMaintenance(ctx context.Context, reg *registry, deps orchestrator.Deps, log *slog.Logger) {
	seen := make(map[string]bool)

	for _, spec := range reg.specs() {
		key := spec.Destination + "\x00" + spec.RepoPath
		if seen[key] {
			continue
		}
		seen[key] = true
		runCheckOne(ctx, spec, deps, log)
	}
}

// runCheckOne builds spec's repo, runs one metadata integrity check, logs the
// outcome exactly as the generic maintenance path did, and writes the record.
func runCheckOne(ctx context.Context, spec *discovery.BackupSpec, deps orchestrator.Deps, log *slog.Logger) {
	repo, err := orchestrator.BuildRepo(spec, deps.Config, deps.Resolver, deps.Master)
	if err != nil {
		log.Error("daemon: maintenance failed", "action", "check", "service", spec.Service, "error", err)
		return
	}

	runtimeName := deps.Config.Runtime
	if runtimeName == "" {
		runtimeName = "docker"
	}

	c := check.Run(ctx, deps.Engine, repo, check.Params{
		Spec:        spec,
		HostID:      deps.HostID,
		RuntimeName: runtimeName,
		Version:     deps.Version,
		Trigger:     "schedule",
		ReadData:    false,
	})

	// Preserve the maintenance path's existing log lines: a pass is a completed
	// maintenance action, a non-pass is a failed one (with the reason as the
	// error), and neither stops the sweep over the other repositories.
	if c.Result == "pass" {
		log.Info("daemon: maintenance completed", "action", "check", "service", spec.Service)
	} else {
		reason := c.Result
		if c.Reason != nil {
			reason = *c.Reason
		}
		log.Error("daemon: maintenance failed", "action", "check", "service", spec.Service, "error", reason)
	}

	// Write the evidence when a state dir and a stable host identity are both
	// available (the same gate the run and verify records use). A failed write
	// only warns: an integrity check's evidence must never take the maintenance
	// sweep down.
	if deps.StateDir == "" || deps.HostID == "" {
		return
	}
	if _, werr := record.WriteCheck(deps.StateDir, c); werr != nil {
		log.Warn("daemon: write check record failed", "service", spec.Service, "check_id", c.CheckID, "error", werr)
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
