// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"github.com/tagwright/ballast/internal/manifest"
)

// runOutcome accumulates the observable facts of a single RunBackup, for the
// machine-readable run record and the backup-time manifest. It is threaded
// through the lifecycle steps and read once at the end. A nil StateDir (and
// so no recording) leaves every field at its zero value and nothing is
// written, which is what keeps a bare RunBackup unchanged.
type runOutcome struct {
	// runID is the ULID identifying this run. Empty only if id generation
	// failed, in which case no per-run state is written.
	runID string

	// stopped reports whether ballast.stop actually stopped the workload for
	// the filesystem pass.
	stopped bool

	// manifest is the backup-time manifest handle, non-nil only when the
	// service had verify configured and a manifest was built and written.
	manifest *manifest.Handle
}
