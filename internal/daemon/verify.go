// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"context"
	"log/slog"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
	"github.com/tagwright/ballast/internal/verify"
)

// verifyJobName is the scheduler job key for a service's local verify schedule,
// distinct from its backup job so both can be registered and removed
// independently.
func verifyJobName(service string) string { return service + " (verify)" }

// scheduleVerify registers a service's optional local verify.schedule as its own
// scheduler job, lower priority than the backup itself in that it is only added
// when explicitly configured. Billet drives verify fleet-wide instead, so this
// exists for single-host operators. It is a no-op unless a verify schedule is
// set and a stable host identity is available (a verify record must carry a
// valid host_id).
func (r *registry) scheduleVerify(sched *schedule.Scheduler, deps orchestrator.Deps, spec *discovery.BackupSpec, log *slog.Logger) {
	if !spec.Verify.Configured || spec.Verify.Schedule == "" {
		return
	}
	if deps.HostID == "" {
		log.Warn("daemon: verify schedule ignored; no stable host identity for the verify record", "service", spec.Service)
		return
	}

	err := sched.Add(schedule.Job{
		Name:     verifyJobName(spec.Service),
		Schedule: spec.Verify.Schedule,
		Run: func(ctx context.Context) {
			runScheduledVerify(ctx, deps, spec, log)
		},
	})
	if err != nil {
		log.Error("daemon: schedule verify job failed", "service", spec.Service, "error", err)
	}
}

// runScheduledVerify runs one scheduled verify for spec. It re-inspects the
// container for its current image and volume layout, builds the repo, and drives
// internal/verify with a schedule trigger. Its failure only logs: a verify is
// evidence-gathering, and a failed verify run (as opposed to a fail verdict) must
// not take the daemon down.
func runScheduledVerify(ctx context.Context, deps orchestrator.Deps, spec *discovery.BackupSpec, log *slog.Logger) {
	container, err := deps.Runtime.Inspect(ctx, spec.ContainerID)
	if err != nil {
		log.Error("daemon: verify inspect container", "service", spec.Service, "error", err)
		return
	}

	repo, err := orchestrator.BuildRepo(spec, deps.Config, deps.Resolver, deps.Master)
	if err != nil {
		log.Error("daemon: verify build repo", "service", spec.Service, "error", err)
		return
	}

	runtimeName := deps.Config.Runtime
	if runtimeName == "" {
		runtimeName = "docker"
	}

	v, err := verify.Run(ctx, spec, container, "latest", verify.Deps{
		Runtime:     deps.Runtime,
		Engine:      deps.Engine,
		Repo:        repo,
		Logger:      log,
		StateDir:    deps.StateDir,
		HostID:      deps.HostID,
		Version:     deps.Version,
		RuntimeName: runtimeName,
		Trigger:     "schedule",
	})
	if err != nil {
		log.Error("daemon: scheduled verify failed to run", "service", spec.Service, "error", err)
		return
	}
	log.Info("daemon: scheduled verify complete", "service", spec.Service, "result", v.Result, "verify_id", v.VerifyID)
}
