// SPDX-License-Identifier: GPL-3.0-or-later

// Package config loads Ballast's daemon configuration: named destinations,
// global defaults for anything a label can also set, and the notification
// and telemetry channel lists. The config file is optional; every scalar
// global default can also be set (or overridden) by a BALLAST_* environment
// variable, so env-only operation works with no file at all.
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
	// are never splayed regardless of this setting.
	Splay bool `yaml:"splay,omitempty"`

	// Retention is the default retention policy string applied when a
	// service sets no ballast.retention.* labels. Overridable by
	// BALLAST_RETENTION.
	Retention string `yaml:"retention,omitempty"`

	// Exclude is the global glob-exclude list, additive to any per-service
	// ballast.exclude labels.
	Exclude []string `yaml:"exclude,omitempty"`

	// DiscoverExclude lists mount name/path patterns skipped during
	// auto-discovery (tmpfs, sockets, localtime-class noise) on top of the
	// runtime's own built-in filters.
	DiscoverExclude []string `yaml:"discover_exclude,omitempty"`

	// HostRoots maps a host-side path prefix Ballast has mounted to the path
	// it appears under inside the Ballast container, so bind-mount sources
	// discovered from container inspection can be resolved to a backupable
	// path.
	HostRoots map[string]string `yaml:"host_roots,omitempty"`

	// SecretsDir is the directory named secrets are resolved from.
	// Overridable by BALLAST_SECRETS_DIR. Defaults to
	// secret.DefaultSecretsDir when unset.
	SecretsDir string `yaml:"secrets_dir,omitempty"`

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
}

// Default values applied to any Config that leaves the corresponding field
// unset, whether that config came from a file, from env, or from neither.
const (
	defaultSchedule    = "@daily"
	defaultConcurrency = 1
	defaultSecretsDir  = "/run/ballast/secrets"
)

// Load reads the YAML config file at path, overlays BALLAST_* environment
// variables onto the global-default scalars (env wins over the file), and
// applies defaults to anything still unset.
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

	applyDefaults(cfg)

	return cfg, nil
}

// overlayEnv applies the BALLAST_* environment variables that back the
// scalar global defaults. Any variable that is set wins over whatever the
// config file (or the zero value) supplied.
func overlayEnv(cfg *Config) error {
	if v, ok := os.LookupEnv("BALLAST_SCHEDULE"); ok {
		cfg.Schedule = v
	}
	if v, ok := os.LookupEnv("BALLAST_WINDOW"); ok {
		cfg.Window = v
	}
	if v, ok := os.LookupEnv("BALLAST_RETENTION"); ok {
		cfg.Retention = v
	}
	if v, ok := os.LookupEnv("BALLAST_DEFAULT_DESTINATION"); ok {
		cfg.DefaultDestination = v
	}
	if v, ok := os.LookupEnv("BALLAST_SECRETS_DIR"); ok {
		cfg.SecretsDir = v
	}
	if v, ok := os.LookupEnv("BALLAST_PRUNE_SCHEDULE"); ok {
		cfg.PruneSchedule = v
	}
	if v, ok := os.LookupEnv("BALLAST_CHECK_SCHEDULE"); ok {
		cfg.CheckSchedule = v
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
}
