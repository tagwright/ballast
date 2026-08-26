// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tagwright/beacon"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/daemon"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/orchestrator"
	"github.com/tagwright/ballast/internal/secret"
	"github.com/tagwright/ballast/pkg/runtime"
)

// defaultDockerSocket is used when DOCKER_HOST names no socket, matching
// internal/daemon's own default.
const defaultDockerSocket = "/var/run/docker.sock"

// commonDeps holds the collaborators every subcommand except "daemon" and
// "key" needs: both of those build their own, narrower set (daemon.Run
// wires itself up internally; key needs only the master secret, and must
// keep working with no Docker socket at all).
type commonDeps struct {
	Config   *config.Config
	Resolver secret.Resolver
	Runtime  runtime.Runtime
	Engine   engine.Engine
	Master   []byte // nil if no master secret is provisioned; per-service ballast.password-secret can still work
	Notifier *beacon.Beacon
	Logger   *slog.Logger
}

// buildCommonDeps loads configPath and wires up the logger, secret
// resolver, Docker runtime, and restic engine, exactly as the daemon does.
// The master secret is loaded best-effort: its absence is not an error
// here (only DeriveRepoPassword, called lazily by BuildRepo, ever needs
// it), mirroring how the daemon itself treats a missing master secret.
func buildCommonDeps(configPath string) (*commonDeps, error) {
	logger, err := newLogger()
	if err != nil {
		return nil, err
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	resolver := secret.FileEnvResolver(cfg.SecretsDir)

	master, err := secret.LoadMaster(resolver)
	if err != nil {
		master = nil
	}

	return &commonDeps{
		Config:   cfg,
		Resolver: resolver,
		Runtime:  runtime.NewDocker(dockerSocket()),
		Engine:   engine.NewRestic(""),
		Master:   master,
		Logger:   logger,
	}, nil
}

// withNotifier builds d.Notifier from d.Config, for the one command
// ("backup") whose run should report its outcome through the same channels
// a scheduled daemon run would. It calls internal/daemon's BuildNotifier
// directly rather than duplicating that wiring here, so the CLI and the
// daemon can never drift apart on how a config maps onto beacon's types.
func (d *commonDeps) withNotifier() error {
	notifier, err := daemon.BuildNotifier(d.Config, d.Resolver)
	if err != nil {
		return fmt.Errorf("build notifier: %w", err)
	}
	d.Notifier = notifier
	return nil
}

// dockerSocket resolves the Docker API socket path the same way
// internal/daemon.dockerSocket does: DOCKER_HOST if set (with a "unix://"
// scheme prefix stripped, since runtime.NewDocker wants a bare path),
// otherwise the conventional default. Duplicated rather than exported from
// internal/daemon, since that package's Run is meant to be the daemon's
// single self-contained wiring path.
func dockerSocket() string {
	if v := os.Getenv("DOCKER_HOST"); v != "" {
		return strings.TrimPrefix(v, "unix://")
	}
	return defaultDockerSocket
}

// discoverService lists rt's containers and returns the BackupSpec for the
// one whose discovered service name equals service. It returns a nil spec
// and a nil error if no container currently resolves to that service name
// (not opted in, or the container simply doesn't exist right now): that is
// the normal disaster-recovery case, and the caller is expected to fall
// back to explicit --destination/--repo-path flags rather than treat it as
// a failure.
func discoverService(ctx context.Context, rt runtime.Runtime, cfg *config.Config, service string) (*discovery.BackupSpec, error) {
	containers, err := rt.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	for _, c := range containers {
		spec, _, err := discovery.Discover(c, cfg)
		if spec != nil && spec.Service == service {
			return spec, err
		}
	}
	return nil, nil
}

// discoverAllServices lists rt's containers and returns the BackupSpec for
// every one that is currently discoverable and enabled. A discovery
// validation error on one container does not stop the others (the daemon's
// own discovery pass is what alerts on that); it is simply dropped here, so
// one misconfigured service never blanks out "ballast snapshots" for every
// other one.
func discoverAllServices(ctx context.Context, rt runtime.Runtime, cfg *config.Config) ([]*discovery.BackupSpec, error) {
	containers, err := rt.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	var specs []*discovery.BackupSpec
	for _, c := range containers {
		spec, _, err := discovery.Discover(c, cfg)
		if spec != nil && err == nil {
			specs = append(specs, spec)
		}
	}
	return specs, nil
}

// resolveRepo resolves the repository for service, the same way for both
// "ballast snapshots" and "ballast restore": if the service's container is
// currently discoverable, its own spec builds the repository, exactly as a
// scheduled backup would build it. Otherwise destFlag and repoPathFlag (the
// command's --destination and --repo-path flags) build a synthetic spec
// directly, which is what makes these commands usable for disaster recovery
// once the container, and its labels, are gone.
func resolveRepo(ctx context.Context, d *commonDeps, service, destFlag, repoPathFlag string) (engine.Repo, error) {
	spec, err := discoverService(ctx, d.Runtime, d.Config, service)
	if err != nil {
		return engine.Repo{}, fmt.Errorf("discover service %q: %w", service, err)
	}

	if spec == nil {
		if destFlag == "" {
			return engine.Repo{}, fmt.Errorf(
				"service %q not found via Docker discovery; pass --destination (and --repo-path, if it differs from the service name) for disaster recovery",
				service)
		}
		repoPath := repoPathFlag
		if repoPath == "" {
			repoPath = service
		}
		spec = &discovery.BackupSpec{
			Service:     service,
			Destination: destFlag,
			RepoPath:    repoPath,
		}
	}

	return orchestrator.BuildRepo(spec, d.Config, d.Resolver, d.Master)
}
