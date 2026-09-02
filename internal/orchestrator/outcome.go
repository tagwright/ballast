// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package orchestrator

import (
	"time"

	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/manifest"
)

// runOutcome accumulates the observable facts of a single RunBackup, for the
// machine-readable run record and the backup-time manifest. It is threaded
// through the lifecycle steps and read once at the end. When recording is off
// (Deps.StateDir empty and Deps.JSON false) the accumulated fields are simply
// never read, so a bare RunBackup stays byte-for-byte unchanged.
type runOutcome struct {
	// runID is the ULID identifying this run. Empty only if id generation
	// failed, in which case no per-run state is written.
	runID string

	// stopped reports whether ballast.stop actually stopped the workload for
	// the filesystem pass.
	stopped bool

	// fsResult is the filesystem pass result, non-nil only when a filesystem
	// backup ran and succeeded.
	fsResult *engine.BackupResult

	// streams holds one outcome per stream.<id>, in the order the streams ran.
	streams []streamOutcome

	// pre and post are the exec hook outcomes, non-nil only when the hook was
	// declared and ran.
	pre  *hookOutcome
	post *hookOutcome

	// retention is the forget pass outcome, non-nil only when forget ran.
	retention *retentionOutcome

	// manifest is the backup-time manifest handle, non-nil only when the
	// service had verify configured and a manifest was built and written.
	manifest *manifest.Handle
}

// streamOutcome is one stream.<id> dump's result.
type streamOutcome struct {
	id       string
	filename string
	// bytes is the raw byte count piped from the dump command into the engine.
	bytes uint64
	// exit is the dump command's exit code (0 on success). It is 1 when the
	// exec could not be started at all, since no real code is available then.
	exit     int
	duration time.Duration
	err      error
	// result is the engine's own accounting for the stream snapshot, set only
	// when a snapshot was written and kept.
	result   engine.BackupResult
	produced bool
}

// hookOutcome is one exec.pre or exec.post hook's result.
type hookOutcome struct {
	exit     int
	duration time.Duration
	err      error
}

// retentionOutcome is the forget pass's result.
type retentionOutcome struct {
	applied bool
	err     error
}
