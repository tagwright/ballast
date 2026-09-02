// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package daemon wires every Ballast package together into the long-running
// backup daemon: it loads config, resolves secrets, builds the notifier, the
// runtime, and the engine, runs an initial discovery pass, watches the
// container socket for lifecycle changes, and drives the scheduler until its
// context is cancelled. The CLI (a separate task) calls Run as its daemon
// entry point.
package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/hostid"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
	"github.com/tagwright/ballast/internal/secret"
	"github.com/tagwright/core/runtime"
)

// defaultDockerSocket is used when cfg.Runtime is "docker" and neither
// cfg.Socket nor DOCKER_HOST names a socket path.
const defaultDockerSocket = "/var/run/docker.sock"

// Run loads configPath, wires up every collaborator, runs an initial
// discovery pass, and then drives the scheduler and the runtime's lifecycle
// watch until ctx is cancelled. Signal handling belongs to the caller: Run
// itself only ever reacts to ctx.
func Run(ctx context.Context, configPath, version string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("daemon: load config: %w", err)
	}

	// The stable host identity gates run-record writing: without it a record
	// would carry no valid host_id, so if it cannot be resolved recording is
	// left off (recordStateDir stays empty) rather than writing invalid
	// records. Backups themselves are unaffected.
	hostID, herr := hostid.LoadOrCreate(cfg.StateDir)
	recordStateDir := cfg.StateDir
	if herr != nil {
		logger.Warn("daemon: host identity unavailable; run records disabled", "error", herr)
		recordStateDir = ""
	}

	resolver := secret.FileEnvResolver(cfg.SecretsDir)

	master, err := secret.LoadMaster(resolver)
	if err != nil {
		logger.Warn("daemon: no master secret available; services deriving their repo password will fail until one is provisioned (a per-service ballast.password-secret can still work)",
			"error", err)
		master = nil
	}

	notifier, err := BuildNotifier(cfg, resolver)
	if err != nil {
		return fmt.Errorf("daemon: build notifier: %w", err)
	}

	rt, err := buildRuntime(cfg)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	defer func() {
		if err := rt.Close(); err != nil {
			logger.Warn("daemon: close runtime", "error", err)
		}
	}()

	eng := engine.NewRestic("")

	schedCfg, err := schedulerConfig(cfg)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	sched, err := schedule.New(schedCfg)
	if err != nil {
		return fmt.Errorf("daemon: build scheduler: %w", err)
	}

	deps := orchestrator.Deps{
		Runtime:  rt,
		Engine:   eng,
		Config:   cfg,
		Resolver: resolver,
		Master:   master,
		Notifier: notifier,
		Logger:   logger,
		StateDir: recordStateDir,
		HostID:   hostID,
		Version:  version,
		Trigger:  "schedule",
	}

	reg := newRegistry()

	if err := discoverAll(ctx, rt, cfg, reg, sched, deps, logger, notifier); err != nil {
		return fmt.Errorf("daemon: initial discovery: %w", err)
	}

	scheduleMaintenance(sched, reg, deps, logger)

	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		watchLoop(ctx, rt, cfg, reg, sched, deps, logger, notifier)
	}()

	sched.Run(ctx)
	<-watchDone

	return nil
}

// buildRuntime constructs the Runtime adapter cfg.Runtime selects: Docker
// (the default) or Podman, both talking to a Docker Engine API-compatible
// socket. Podman's own default-socket resolution (the rootless
// XDG_RUNTIME_DIR path, falling back to the rootful system socket) is left
// to runtime.NewPodman when cfg.Socket and CONTAINER_HOST are both unset,
// so that logic lives in one place.
func buildRuntime(cfg *config.Config) (runtime.Runtime, error) {
	switch cfg.Runtime {
	case "", "docker":
		return runtime.NewDocker(dockerSocket(cfg)), nil
	case "podman":
		return runtime.NewPodman(podmanSocket(cfg)), nil
	default:
		return nil, fmt.Errorf("unknown runtime %q, want \"docker\" or \"podman\"", cfg.Runtime)
	}
}

// dockerSocket resolves the Docker API socket path: cfg.Socket if set,
// otherwise DOCKER_HOST (with a "unix://" scheme prefix stripped, since
// NewDocker wants a bare path), otherwise the conventional default.
func dockerSocket(cfg *config.Config) string {
	if cfg.Socket != "" {
		return cfg.Socket
	}
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		return strings.TrimPrefix(v, "unix://")
	}
	return defaultDockerSocket
}

// podmanSocket resolves the Podman API socket path: cfg.Socket if set,
// otherwise CONTAINER_HOST (with a "unix://" scheme prefix stripped, the
// same convention podman-remote itself uses), otherwise empty, which tells
// runtime.NewPodman to fall back to its own rootless/rootful default.
func podmanSocket(cfg *config.Config) string {
	if cfg.Socket != "" {
		return cfg.Socket
	}
	if v := os.Getenv("CONTAINER_HOST"); v != "" {
		return strings.TrimPrefix(v, "unix://")
	}
	return ""
}

// schedulerConfig maps cfg onto schedule.Config: cfg.Window is a single
// "HH:MM-HH:MM" string (per the label grammar's Fork 6) that schedule.Config
// wants split into its two clock-time bounds. cfg.Splay carries straight
// through: both are a *bool defaulting to "splay on" when nil, and cfg has
// already been through config.Load's applyDefaults by the time Run calls
// this, so cfg.Splay is never actually nil here in practice.
func schedulerConfig(cfg *config.Config) (schedule.Config, error) {
	var start, end string
	if cfg.Window != "" {
		parts := strings.SplitN(cfg.Window, "-", 2)
		if len(parts) != 2 {
			return schedule.Config{}, fmt.Errorf("invalid window %q, want HH:MM-HH:MM", cfg.Window)
		}
		start, end = parts[0], parts[1]
	}

	return schedule.Config{
		WindowStart: start,
		WindowEnd:   end,
		Concurrency: cfg.Concurrency,
		Splay:       cfg.Splay,
	}, nil
}
