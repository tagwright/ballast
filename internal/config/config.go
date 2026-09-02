// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package config loads Ballast's daemon configuration: named destinations,
// global defaults for anything a label can also set, and the notification
// and telemetry channel lists. The config file is optional; every global
// default (scalar, list, or map) can also be set (or overridden) by a
// BALLAST_* environment variable, so env-only operation works with no file
// at all.
//
// Config never holds a literal secret value. Destination.Env and the
// notification/telemetry Settings maps name secrets (bare logical names,
// resolved later through a secret.Resolver); URLs and credentials are kept
// out of labels entirely per the label grammar's Fork 3.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Destination is one named backup target from the "destinations" map in
// ballast.yml. A service selects one by name via the ballast.repo label.
type Destination struct {
	// URL is the engine-native repository location, e.g. "/repos" or
	// "s3:https://acc.r2.cloudflarestorage.com/bucket". Engine syntax is
	// deliberately quarantined here, out of the label grammar.
	URL string `yaml:"url"`

	// Env maps an environment variable name the engine's child process needs
	// (e.g. "AWS_ACCESS_KEY_ID") to a named secret resolved at use time. The
	// map value is a secret name, never the literal credential.
	Env map[string]string `yaml:"env,omitempty"`
}

// ChannelConfig is a plain mirror of a notification channel's shape from the
// beacon notification module. Ballast does not import beacon here: the
// orchestrator maps ChannelConfig onto beacon's own config type when it
// wires the notifier up. Settings values that are secrets are named, not
// literal.
type ChannelConfig struct {
	// Type selects the backend, e.g. "smtp", "ntfy", "discord", "webhook".
	Type string `yaml:"type"`

	// MinLevel is the minimum severity this channel fires on, e.g. "warn".
	MinLevel string `yaml:"min_level,omitempty"`

	// Settings carries backend-specific config. Values that are secrets are
	// named (resolved through a secret.Resolver later), never literal.
	Settings map[string]string `yaml:"settings,omitempty"`
}

// TelemetryConfig is a plain mirror of a telemetry sink's shape from the
// beacon module, e.g. the Gatus external-endpoint push. Same non-import,
// same secret-naming rule as ChannelConfig.
type TelemetryConfig struct {
	// Type selects the sink, e.g. "gatus".
	Type string `yaml:"type"`

	// Settings carries backend-specific config. Values that are secrets are
	// named, never literal.
	Settings map[string]string `yaml:"settings,omitempty"`
}

// LogConfig configures Ballast's own logging.
type LogConfig struct {
	// Format selects the slog handler: "text" (the default) or "json".
	// Overridable by BALLAST_LOG_FORMAT.
	Format string `yaml:"format,omitempty"`
}

// Config is Ballast's daemon configuration: named destinations plus the
// global defaults every label-level setting falls back to when a service
// does not override it.
type Config struct {
	// Destinations is the named backend map keyed by destination name, e.g.
	// "local" or "r2". Selected per service via ballast.repo.
	Destinations map[string]Destination `yaml:"destinations,omitempty"`

	// DefaultDestination is the destination name used when a service sets no
	// ballast.repo label. Overridable by BALLAST_DEFAULT_DESTINATION.
	DefaultDestination string `yaml:"default_destination,omitempty"`

	// Schedule is the default cron/alias applied when a service sets no
	// ballast.schedule label. Overridable by BALLAST_SCHEDULE.
	Schedule string `yaml:"schedule,omitempty"`

	// Window is the splay window period-alias schedules are spread across
	// (e.g. "01:00-05:00"), per the grammar's Fork 6. Overridable by
	// BALLAST_WINDOW.
	Window string `yaml:"window,omitempty"`

	// Splay turns the deterministic per-service splay of period aliases
	// (@daily, @hourly, ...) on or off. Raw cron and "@every <dur>" schedules
	// are never splayed regardless of this setting. Overridable by
	// BALLAST_SPLAY.
	//
	// Splay is a *bool, not a bool, specifically so applyDefaults can tell
	// "never set" (nil: defaults to true, splay stays on, matching every
	// other doc comment's description of the feature) apart from "explicitly
	// set to false" (splay: false in ballast.yml, or BALLAST_SPLAY=false):
	// a plain bool's zero value would be indistinguishable from an explicit
	// false, which would silently disable the anti-stampede splay by
	// default. Callers past Load always see a non-nil pointer.
	Splay *bool `yaml:"splay,omitempty"`

	// Retention is the default retention policy string applied when a
	// service sets no ballast.retention.* labels. Overridable by
	// BALLAST_RETENTION.
	Retention string `yaml:"retention,omitempty"`

	// Exclude is the global glob-exclude list, additive to any per-service
	// ballast.exclude labels. Overridable by BALLAST_EXCLUDE, a
	// comma-separated list.
	Exclude []string `yaml:"exclude,omitempty"`

	// DiscoverExclude lists mount name/path patterns skipped during
	// auto-discovery (tmpfs, sockets, localtime-class noise) on top of the
	// runtime's own built-in filters. Overridable by
	// BALLAST_DISCOVER_EXCLUDE, a comma-separated list.
	DiscoverExclude []string `yaml:"discover_exclude,omitempty"`

	// HostRoots maps a host-side path prefix Ballast has mounted to the path
	// it appears under inside the Ballast container, so bind-mount sources
	// discovered from container inspection can be resolved to a backupable
	// path. Overridable by BALLAST_HOST_ROOTS, a comma-separated list of
	// "host=mount" pairs.
	HostRoots map[string]string `yaml:"host_roots,omitempty"`

	// SecretsDir is the directory named secrets are resolved from.
	// Overridable by BALLAST_SECRETS_DIR. Defaults to
	// secret.DefaultSecretsDir when unset.
	SecretsDir string `yaml:"secrets_dir,omitempty"`

	// StateDir is the directory Ballast persists cross-restart state in: the
	// stable host identity, machine-readable run records, and backup-time
	// manifests. Unlike SecretsDir it must outlive a container recreation, so
	// its default is under /var/lib rather than /run. Overridable by
	// BALLAST_STATE_DIR.
	StateDir string `yaml:"state_dir,omitempty"`

	// EnableExec is the global gate for exec.pre, exec.post, and stream
	// labels. Overridable by BALLAST_ENABLE_EXEC. Defaults to false.
	EnableExec bool `yaml:"enable_exec,omitempty"`

	// EnableStop is the global gate for the ballast.stop label. Overridable
	// by BALLAST_ENABLE_STOP. Defaults to false.
	EnableStop bool `yaml:"enable_stop,omitempty"`

	// PruneSchedule is the cron/alias schedule prune runs on, global and not
	// per service. Overridable by BALLAST_PRUNE_SCHEDULE.
	PruneSchedule string `yaml:"prune_schedule,omitempty"`

	// CheckSchedule is the cron/alias schedule check runs on, global and not
	// per service. Overridable by BALLAST_CHECK_SCHEDULE.
	CheckSchedule string `yaml:"check_schedule,omitempty"`

	// Concurrency caps how many service backups the orchestrator runs at
	// once. Overridable by BALLAST_CONCURRENCY. Defaults to 1 (serial),
	// matching the grammar's disk/uplink-protection intent.
	Concurrency int `yaml:"concurrency,omitempty"`

	// Notifications is the list of alert channels, each fired per the
	// orchestrator's Notifier seam.
	Notifications []ChannelConfig `yaml:"notifications,omitempty"`

	// Telemetry is the list of health/status push sinks, e.g. Gatus.
	Telemetry []TelemetryConfig `yaml:"telemetry,omitempty"`

	// Runtime selects the container engine Ballast talks to: "docker" or
	// "podman". Overridable by BALLAST_RUNTIME. Defaults to "docker".
	Runtime string `yaml:"runtime,omitempty"`

	// Log configures Ballast's own logging (the handler format).
	Log LogConfig `yaml:"log,omitempty"`

	// Socket is the API socket path for whichever engine Runtime selects.
	// Overridable by BALLAST_SOCKET. Empty means "resolve the engine's own
	// conventional default": internal/daemon and internal/cli fall back to
	// DOCKER_HOST (docker) or CONTAINER_HOST (podman) when set, and
	// otherwise to /var/run/docker.sock for docker or the rootless/rootful
	// podman.sock path runtime.NewPodman resolves on its own for podman.
	Socket string `yaml:"socket,omitempty"`
}

// Default values applied to any Config that leaves the corresponding field
// unset, whether that config came from a file, from env, or from neither.
const (
	defaultSchedule    = "@daily"
	defaultConcurrency = 1
	defaultSecretsDir  = "/run/ballast/secrets"
	defaultStateDir    = "/var/lib/ballast"
	defaultRuntime     = "docker"
	defaultLogFormat   = "text"
)

// defaultDockerVolumesRoot is the standard Docker named-volume data root.
// Ballast's recommended deploy mounts this same path into the Ballast
// container at the same location
// (/var/lib/docker/volumes:/var/lib/docker/volumes:ro), so a named volume's
// host-side mount source (/var/lib/docker/volumes/<name>/_data) already
// resolves to a path Ballast can read with no host_roots configuration at
// all.
//
// This default targets Docker's standard layout. Rootless Podman and any
// installation with a custom Docker data-root (dockerd --data-root) still
// need an explicit host_roots entry pointing at wherever their volumes
// actually live.
const defaultDockerVolumesRoot = "/var/lib/docker/volumes"

// Load reads the YAML config file at path, overlays BALLAST_* environment
// variables onto the global defaults (env wins over the file), and applies
// defaults to anything still unset.
//
// path is optional: an empty path, or a path that does not exist, is not an
// error. Load returns a default Config in that case, so env-only operation
// (no config file at all) works.
func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("config: read %s: %w", path, err)
			}
		} else if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	if err := overlayEnv(cfg); err != nil {
		return nil, err
	}

	if cfg.Runtime != "" && cfg.Runtime != "docker" && cfg.Runtime != "podman" {
		return nil, fmt.Errorf("config: runtime %q, want \"docker\" or \"podman\"", cfg.Runtime)
	}

	if cfg.Log.Format != "" && cfg.Log.Format != "text" && cfg.Log.Format != "json" {
		return nil, fmt.Errorf("config: log.format %q, want \"text\" or \"json\"", cfg.Log.Format)
	}

	applyDefaults(cfg)

	return cfg, nil
}

// overlayEnv applies the BALLAST_* environment variables that back the
// global defaults. Any variable that is set wins over whatever the config
// file (or the zero value) supplied.
func overlayEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("BALLAST_SCHEDULE"); ok {
		cfg.Schedule = v
	}
	if v, ok := os.LookupEnv("BALLAST_WINDOW"); ok {
		cfg.Window = v
	}
	if v, ok := os.LookupEnv("BALLAST_SPLAY"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: BALLAST_SPLAY: %w", err)
		}
		cfg.Splay = &b
	}
	if v, ok := os.LookupEnv("BALLAST_RETENTION"); ok {
		cfg.Retention = v
	}
	if v, ok := os.LookupEnv("BALLAST_EXCLUDE"); ok {
		cfg.Exclude = splitEnvList(v)
	}
	if v, ok := os.LookupEnv("BALLAST_DISCOVER_EXCLUDE"); ok {
		cfg.DiscoverExclude = splitEnvList(v)
	}
	if v, ok := os.LookupEnv("BALLAST_HOST_ROOTS"); ok {
		hostRoots, err := parseHostRootsEnv(v)
		if err != nil {
			return fmt.Errorf("config: BALLAST_HOST_ROOTS: %w", err)
		}
		cfg.HostRoots = hostRoots
	}
	if v, ok := os.LookupEnv("BALLAST_DEFAULT_DESTINATION"); ok {
		cfg.DefaultDestination = v
	}
	if v, ok := os.LookupEnv("BALLAST_SECRETS_DIR"); ok {
		cfg.SecretsDir = v
	}
	if v, ok := os.LookupEnv("BALLAST_STATE_DIR"); ok {
		cfg.StateDir = v
	}
	if v, ok := os.LookupEnv("BALLAST_LOG_FORMAT"); ok {
		cfg.Log.Format = v
	}
	if v, ok := os.LookupEnv("BALLAST_PRUNE_SCHEDULE"); ok {
		cfg.PruneSchedule = v
	}
	if v, ok := os.LookupEnv("BALLAST_CHECK_SCHEDULE"); ok {
		cfg.CheckSchedule = v
	}
	if v, ok := os.LookupEnv("BALLAST_RUNTIME"); ok {
		cfg.Runtime = v
	}
	if v, ok := os.LookupEnv("BALLAST_SOCKET"); ok {
		cfg.Socket = v
	}

	if v, ok := os.LookupEnv("BALLAST_ENABLE_EXEC"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: BALLAST_ENABLE_EXEC: %w", err)
		}
		cfg.EnableExec = b
	}
	if v, ok := os.LookupEnv("BALLAST_ENABLE_STOP"); ok {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config: BALLAST_ENABLE_STOP: %w", err)
		}
		cfg.EnableStop = b
	}
	if v, ok := os.LookupEnv("BALLAST_CONCURRENCY"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: BALLAST_CONCURRENCY: %w", err)
		}
		cfg.Concurrency = n
	}

	return nil
}

// splitEnvList splits a comma-separated BALLAST_* environment value into a
// list, trimming whitespace and dropping empty elements. An empty value
// (the variable set to "") yields nil, clearing whatever the config file
// supplied.
func splitEnvList(v string) []string {
	if v == "" {
		return nil
	}
	fields := strings.Split(v, ",")
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f != "" {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseHostRootsEnv parses a BALLAST_HOST_ROOTS value: a comma-separated
// list of "host=mount" pairs, the env equivalent of the host_roots map in
// ballast.yml. An empty value yields an empty (non-nil) map, so
// applyDefaults still seeds it with the default Docker volumes root.
func parseHostRootsEnv(v string) (map[string]string, error) {
	out := make(map[string]string)
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid pair %q, want host=mount", pair)
		}
		host := strings.TrimSpace(parts[0])
		mount := strings.TrimSpace(parts[1])
		if host == "" || mount == "" {
			return nil, fmt.Errorf("invalid pair %q, want host=mount", pair)
		}
		out[host] = mount
	}
	return out, nil
}

// applyDefaults fills in the sane defaults for anything still unset after
// the file and env passes.
func applyDefaults(cfg *Config) {
	if cfg.Schedule == "" {
		cfg.Schedule = defaultSchedule
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = defaultConcurrency
	}
	if cfg.SecretsDir == "" {
		cfg.SecretsDir = defaultSecretsDir
	}
	if cfg.StateDir == "" {
		cfg.StateDir = defaultStateDir
	}
	if cfg.Log.Format == "" {
		cfg.Log.Format = defaultLogFormat
	}
	if cfg.Runtime == "" {
		cfg.Runtime = defaultRuntime
	}
	if cfg.Splay == nil {
		splayEnabled := true
		cfg.Splay = &splayEnabled
	}
	cfg.HostRoots = withDefaultHostRoots(cfg.HostRoots)
}

// withDefaultHostRoots always seeds the standard Docker named-volume root
// mapped to itself as a baseline, so filesystem auto-discovery resolves
// named-volume mounts out of the box even with an otherwise-empty config:
// the README's "add one label" promise depends on HostRoots never being
// empty by default. Any host_roots entries the user configures in
// ballast.yml are merged on top of (never replace) this baseline, so a user
// who adds a bind-mount root does not lose named-volume discovery.
func withDefaultHostRoots(existing map[string]string) map[string]string {
	merged := map[string]string{defaultDockerVolumesRoot: defaultDockerVolumesRoot}
	for k, v := range existing {
		merged[k] = v
	}
	return merged
}
