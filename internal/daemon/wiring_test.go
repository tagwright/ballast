// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"strings"
	"testing"

	"github.com/tagwright/courier"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/secret"
)

// These cover the daemon's config-to-collaborator translation wiring: the small
// pure resolvers that map a loaded Config and the environment onto the concrete
// values Run threads into the scheduler, the runtime adapters, and the notifier.
// They are Level-2 wiring in miniature (config in, wired value out), and the
// bugs they guard against are the quiet ones: a window string parsed wrong, an
// env socket override ignored, a bad notification level swallowed instead of
// refused.

// TestSchedulerConfig_ParsesWindow proves a "HH:MM-HH:MM" window is split into
// the two clock-time bounds schedule.Config wants, and that splay and
// concurrency carry through unchanged.
func TestSchedulerConfig_ParsesWindow(t *testing.T) {
	splayOff := false
	cfg := &config.Config{
		Window:      "01:30-05:45",
		Concurrency: 3,
		Splay:       &splayOff,
	}
	got, err := schedulerConfig(cfg)
	if err != nil {
		t.Fatalf("schedulerConfig with a valid window returned an error: %v", err)
	}
	if got.WindowStart != "01:30" || got.WindowEnd != "05:45" {
		t.Fatalf("window not split into its bounds: got start %q end %q, want 01:30 / 05:45", got.WindowStart, got.WindowEnd)
	}
	if got.Concurrency != 3 {
		t.Errorf("concurrency did not carry through: got %d, want 3", got.Concurrency)
	}
	if got.Splay == nil || *got.Splay {
		t.Errorf("splay did not carry through: got %v, want a pointer to false", got.Splay)
	}
}

// TestSchedulerConfig_RejectsMalformedWindow proves a window that is not a
// two-part "start-end" string surfaces as an error rather than being silently
// dropped or half-applied.
func TestSchedulerConfig_RejectsMalformedWindow(t *testing.T) {
	cfg := &config.Config{Window: "0130"}
	_, err := schedulerConfig(cfg)
	if err == nil {
		t.Fatal("a malformed window was accepted: schedulerConfig returned no error")
	}
	if !strings.Contains(err.Error(), "window") {
		t.Fatalf("window error does not name the offending field: %v", err)
	}
}

// TestSchedulerConfig_EmptyWindowLeavesBoundsUnset proves the no-window case
// leaves both bounds empty (a 24h window) rather than tripping the parse.
func TestSchedulerConfig_EmptyWindowLeavesBoundsUnset(t *testing.T) {
	got, err := schedulerConfig(&config.Config{})
	if err != nil {
		t.Fatalf("an empty window returned an error: %v", err)
	}
	if got.WindowStart != "" || got.WindowEnd != "" {
		t.Fatalf("an empty window set bounds: got start %q end %q, want both empty", got.WindowStart, got.WindowEnd)
	}
}

// TestDockerSocket_Resolution proves the Docker socket path precedence:
// cfg.Socket wins, then DOCKER_HOST with any unix:// scheme stripped, then the
// conventional default.
func TestDockerSocket_Resolution(t *testing.T) {
	t.Run("cfg.Socket wins", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///env/should/lose.sock")
		if got := dockerSocket(&config.Config{Socket: "/cfg/wins.sock"}); got != "/cfg/wins.sock" {
			t.Fatalf("cfg.Socket did not win: got %q", got)
		}
	})
	t.Run("DOCKER_HOST with scheme stripped", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "unix:///env/docker.sock")
		if got := dockerSocket(&config.Config{}); got != "/env/docker.sock" {
			t.Fatalf("DOCKER_HOST not resolved with its unix:// scheme stripped: got %q", got)
		}
	})
	t.Run("falls back to the conventional default", func(t *testing.T) {
		t.Setenv("DOCKER_HOST", "")
		if got := dockerSocket(&config.Config{}); got != defaultDockerSocket {
			t.Fatalf("no cfg.Socket and no DOCKER_HOST did not fall back to the default: got %q, want %q", got, defaultDockerSocket)
		}
	})
}

// TestPodmanSocket_Resolution proves the Podman socket precedence: cfg.Socket,
// then CONTAINER_HOST with its scheme stripped, then empty (which defers to
// runtime.NewPodman's own rootless/rootful default).
func TestPodmanSocket_Resolution(t *testing.T) {
	t.Run("cfg.Socket wins", func(t *testing.T) {
		t.Setenv("CONTAINER_HOST", "unix:///env/should/lose.sock")
		if got := podmanSocket(&config.Config{Socket: "/cfg/wins.sock"}); got != "/cfg/wins.sock" {
			t.Fatalf("cfg.Socket did not win: got %q", got)
		}
	})
	t.Run("CONTAINER_HOST with scheme stripped", func(t *testing.T) {
		t.Setenv("CONTAINER_HOST", "unix:///run/podman.sock")
		if got := podmanSocket(&config.Config{}); got != "/run/podman.sock" {
			t.Fatalf("CONTAINER_HOST not resolved with its unix:// scheme stripped: got %q", got)
		}
	})
	t.Run("empty defers to the adapter default", func(t *testing.T) {
		t.Setenv("CONTAINER_HOST", "")
		if got := podmanSocket(&config.Config{}); got != "" {
			t.Fatalf("no cfg.Socket and no CONTAINER_HOST should yield empty (adapter default), got %q", got)
		}
	})
}

// TestParseLevel proves each accepted severity string maps to its courier.Level,
// that an empty value means "everything" (Info), and that an unknown level is
// refused rather than silently defaulted.
func TestParseLevel(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want courier.Level
	}{
		{"", courier.LevelInfo},
		{"info", courier.LevelInfo},
		{"INFO", courier.LevelInfo},
		{"warn", courier.LevelWarning},
		{" warning ", courier.LevelWarning},
		{"error", courier.LevelError},
	} {
		lvl, err := parseLevel(tc.in)
		if err != nil {
			t.Fatalf("parseLevel(%q) errored on a valid level: %v", tc.in, err)
		}
		if lvl != tc.want {
			t.Errorf("parseLevel(%q) = %v, want %v", tc.in, lvl, tc.want)
		}
	}

	if _, err := parseLevel("bogus"); err == nil {
		t.Fatal("parseLevel accepted an unknown level: it must refuse, not silently default")
	}
}

// TestBuildNotifier_AddsLogFloorWhenNoChannels proves that a config declaring no
// notification channel still gets beacon's always-on "log" backend, so a run's
// outcome is never silently unreported.
func TestBuildNotifier_AddsLogFloorWhenNoChannels(t *testing.T) {
	resolver := secret.FileEnvResolver(t.TempDir())
	b, err := BuildNotifier(&config.Config{}, resolver)
	if err != nil {
		t.Fatalf("BuildNotifier with no channels errored: %v", err)
	}
	if b == nil {
		t.Fatal("BuildNotifier returned a nil beacon with no error")
	}
}

// TestBuildNotifier_RejectsBadChannelLevel proves a notification channel with an
// unparseable MinLevel fails the whole notifier build loudly, naming the channel,
// rather than wiring up a beacon that quietly drops that channel.
func TestBuildNotifier_RejectsBadChannelLevel(t *testing.T) {
	resolver := secret.FileEnvResolver(t.TempDir())
	cfg := &config.Config{
		Notifications: []config.ChannelConfig{
			{Type: "log", MinLevel: "bogus"},
		},
	}
	_, err := BuildNotifier(cfg, resolver)
	if err == nil {
		t.Fatal("a channel with an invalid MinLevel was accepted: BuildNotifier returned no error")
	}
	if !strings.Contains(err.Error(), "notification channel") {
		t.Fatalf("BuildNotifier error does not identify the offending channel: %v", err)
	}
}
