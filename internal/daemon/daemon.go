// SPDX-License-Identifier: GPL-3.0-or-later

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
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/schedule"
	"github.com/tagwright/ballast/internal/secret"
	"github.com/tagwright/ballast/pkg/runtime"
)

// defaultDockerSocket is used when neither DOCKER_HOST nor any future
// config field names a socket path.
const defaultDockerSocket = "/var/run/docker.sock"

// Run loads configPath, wires up every collaborator, runs an initial
// discovery pass, and then drives the scheduler and the runtime's lifecycle
// watch until ctx is cancelled. Signal handling belongs to the caller: Run
// itself only ever reacts to ctx.
func Run(ctx context.Context, configPath string, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("daemon: load config: %w", err)
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

	rt := runtime.NewDocker(dockerSocket())
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

// dockerSocket resolves the Docker API socket path: DOCKER_HOST if set
// (with a "unix://" scheme prefix stripped, since NewDocker wants a bare
// path), otherwise the conventional default.
func dockerSocket() string {
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		return strings.TrimPrefix(v, "unix://")
	}
	return defaultDockerSocket
}

// schedulerConfig maps cfg onto schedule.Config: cfg.Window is a single
// "HH:MM-HH:MM" string (per the label grammar's Fork 6) that schedule.Config
// wants split into its two clock-time bounds.
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
	}, nil
}
