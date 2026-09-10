// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"fmt"
	"strings"

	"github.com/tagwright/courier"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/secret"
)

// BuildNotifier maps cfg's notification channels and telemetry sinks onto
// beacon's own config types and builds a Beacon. If cfg configures no
// notification channel at all, beacon's built-in "log" backend is added as
// the always-on floor, so a run's outcome is never silently unreported.
//
// This is the single shared wiring path: the daemon uses it to build the
// notifier a scheduled run reports through, and the CLI's "ballast backup"
// uses it (via commonDeps.withNotifier) so an ad hoc run reports through
// exactly the same channels.
func BuildNotifier(cfg *config.Config, resolver secret.Resolver) (*courier.Beacon, error) {
	channels := make([]courier.ChannelConfig, 0, len(cfg.Notifications))
	for i, c := range cfg.Notifications {
		level, err := parseLevel(c.MinLevel)
		if err != nil {
			return nil, fmt.Errorf("daemon: notification channel %d (%s): %w", i, c.Type, err)
		}
		channels = append(channels, courier.ChannelConfig{
			Type:     c.Type,
			MinLevel: level,
			Settings: c.Settings,
		})
	}
	if len(channels) == 0 {
		channels = append(channels, courier.ChannelConfig{Type: "log"})
	}

	telemetry := make([]courier.TelemetryConfig, 0, len(cfg.Telemetry))
	for _, t := range cfg.Telemetry {
		telemetry = append(telemetry, courier.TelemetryConfig{
			Type:     t.Type,
			Settings: t.Settings,
		})
	}

	beaconCfg := courier.Config{Channels: channels, Telemetry: telemetry}
	return courier.New(beaconCfg, courier.SecretResolver(resolver))
}

// parseLevel maps a config.ChannelConfig.MinLevel string onto a
// courier.Level. An empty value means "receive everything" (LevelInfo).
func parseLevel(s string) (courier.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return courier.LevelInfo, nil
	case "warn", "warning":
		return courier.LevelWarning, nil
	case "error":
		return courier.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown notification level %q", s)
	}
}
