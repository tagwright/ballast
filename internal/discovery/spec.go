// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package discovery

import (
	"time"

	"github.com/tagwright/ballast/internal/engine"
)

// BackupSpec is one service's fully resolved backup configuration, produced
// by Discover from a container's labels plus Ballast's global config. It is
// everything the orchestrator needs to run a service: nothing downstream
// re-reads labels.
type BackupSpec struct {
	Service       string // resolved service identity (ballast.name, compose service, or container name)
	Project       string // compose project, empty if the container is not a compose service
	ContainerID   string
	ContainerName string
	Image         string // the discovered container's image reference, used as the default throwaway image for container/stream-restore verify

	Destination    string // ballast.repo, defaults to config.DefaultDestination
	RepoPath       string // ballast.repo.path, defaults to Service
	PasswordSecret string // ballast.password-secret, empty means derive from the master key

	Paths   []string     // resolved host-visible paths for filesystem backup
	Streams []StreamSpec // exec-stdout-to-stdin dumps

	ExecPre  *HookSpec // ballast.exec.pre, nil if unset
	ExecPost *HookSpec // ballast.exec.post, nil if unset
	Stop     bool      // ballast.stop

	Schedule string // ballast.schedule; empty means the caller applies the global default

	// Retention is exactly what the service's ballast.retention.* labels say.
	// If no retention label was present on the container, this is the zero
	// value of engine.RetentionPolicy, and the caller (not this package)
	// applies the global default policy: per the grammar's Fork 2, a labeled
	// policy always replaces the global one wholesale, it never merges with
	// it, so this package never mixes the two.
	Retention engine.RetentionPolicy

	Tags          []string // ballast.tags, appended to Ballast's own auto tags by the caller
	Excludes      []string // ballast.exclude / ballast.exclude.<n>
	ExcludeCaches bool     // ballast.exclude-caches, defaults to true

	// NotifySuppress is ballast.notify.suppress: when true, the orchestrator
	// skips the beacon Notify call for this service entirely (mutes alert
	// channels), but never affects the telemetry Report call, since a health
	// push is not an alert.
	NotifySuppress bool
	// NotifyOnSuccess is ballast.notify.on-success: when true, a successful
	// backup notifies at beacon.LevelWarning instead of the default
	// LevelInfo, so it surfaces on channels configured to only forward
	// warnings and errors. Failures are unaffected; they already notify at
	// LevelError.
	NotifyOnSuccess bool

	// VerifyConfigured reports whether the service carries any verify.* label.
	// It is the opt-in trigger for the backup-time manifest, which costs a hash
	// pass over the volume and so is never built for a service that has no
	// verify configured. It mirrors Verify.Configured.
	VerifyConfigured bool

	// Verify is the service's fully resolved verify configuration, parsed from
	// its verify.* labels. Its fields are only meaningful when
	// Verify.Configured (equivalently VerifyConfigured) is true; the defaults
	// (files mode, 10m timeout) are filled regardless so a reader can consult
	// Mode and Timeout unconditionally.
	Verify VerifySpec
}

// StreamSpec is one ballast.stream.<id> dump: a command run inside the
// service's container whose stdout is piped straight into the engine as a
// stdin snapshot.
type StreamSpec struct {
	ID       string
	Command  string
	Filename string
	User     string
	Timeout  time.Duration
}

// HookSpec is one exec.pre or exec.post consistency hook.
type HookSpec struct {
	Command string
	User    string
	Timeout time.Duration
}
