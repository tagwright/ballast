// SPDX-License-Identifier: GPL-3.0-or-later

package daemon

import (
	"fmt"
	"strings"

	"github.com/tagwright/beacon"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/secret"
)

// buildNotifier maps cfg's notification channels and telemetry sinks onto
// beacon's own config types and builds a Beacon. If cfg configures no
// notification channel at all, beacon's built-in "log" backend is added as
// the always-on floor, so a run's outcome is never silently unreported.
func buildNotifier(cfg *config.Config, resolver secret.Resolver) (*beacon.Beacon, error) {
	channels := make([]beacon.ChannelConfig, 0, len(cfg.Notifications))
	for i, c := range cfg.Notifications {
		level, err := parseLevel(c.MinLevel)
		if err != nil {
			return nil, fmt.Errorf("daemon: notification channel %d (%s): %w", i, c.Type, err)
		}
		channels = append(channels, beacon.ChannelConfig{
			Type:     c.Type,
			MinLevel: level,
			Settings: c.Settings,
		})
	}
	if len(channels) == 0 {
		channels = append(channels, beacon.ChannelConfig{Type: "log"})
	}

	telemetry := make([]beacon.TelemetryConfig, 0, len(cfg.Telemetry))
	for _, t := range cfg.Telemetry {
		telemetry = append(telemetry, beacon.TelemetryConfig{
			Type:     t.Type,
			Settings: t.Settings,
		})
	}

	beaconCfg := beacon.Config{Channels: channels, Telemetry: telemetry}
	return beacon.New(beaconCfg, beacon.SecretResolver(resolver))
}

// parseLevel maps a config.ChannelConfig.MinLevel string onto a
// beacon.Level. An empty value means "receive everything" (LevelInfo).
func parseLevel(s string) (beacon.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return beacon.LevelInfo, nil
	case "warn", "warning":
		return beacon.LevelWarning, nil
	case "error":
		return beacon.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown notification level %q", s)
	}
}
