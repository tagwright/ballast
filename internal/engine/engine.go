// SPDX-License-Identifier: GPL-3.0-or-later

// Package engine defines the backup engine Ballast drives, and the restic
// implementation of it. Ballast owns discovery, scheduling, and orchestration;
// the engine owns the bytes. The interface is intentionally small and functional
// (no restic-specific vocabulary leaks into the method set) so a second driver,
// most likely kopia, can slot in later without forcing a relabel or a rewrite.
//
// One repository per service is the model: every Repo here is a single service's
// isolated repository, with its own password and its own lock domain.
package engine

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotImplemented is returned by driver methods that are not wired up yet.
var ErrNotImplemented = errors.New("engine: not implemented")

// Engine is a backup engine Ballast can drive. Implementations shell out to a
// proven backup tool and parse its structured output. They must not lose or
// mutate data on their own initiative: Ballast decides when to forget and prune.
type Engine interface {
	// EnsureRepo initializes the repository if it does not yet exist, and is a
	// no-op if it does.
	EnsureRepo(ctx context.Context, repo Repo) error

	// Backup writes one snapshot. Exactly one source is set on the request:
	// Paths for a filesystem backup, or Stdin for a streamed dump.
	Backup(ctx context.Context, req BackupRequest) (BackupResult, error)

	// Forget applies a retention policy, deleting snapshots the policy does not
	// keep. It does not prune repository data; that is a separate step.
	Forget(ctx context.Context, repo Repo, policy RetentionPolicy) error

	// DeleteSnapshot removes a single snapshot by ID, independent of any
	// retention policy. It exists for compensating cleanup: a stream backup
	// whose dump command failed after the engine had already begun (or even
	// finished) writing a snapshot from its partial stdout must not leave
	// that truncated snapshot behind. It does not prune repository data.
	DeleteSnapshot(ctx context.Context, repo Repo, id string) error

	// Prune reclaims space from data no snapshot references.
	Prune(ctx context.Context, repo Repo) error

	// Check verifies repository integrity.
	Check(ctx context.Context, repo Repo, readData bool) error

	// Snapshots lists the snapshots in a repository.
	Snapshots(ctx context.Context, repo Repo) ([]Snapshot, error)

	// Restore restores a snapshot to a target directory.
	Restore(ctx context.Context, req RestoreRequest) error

	// Name identifies the driver, e.g. "restic".
	Name() string
}

// Repo is one service's isolated repository.
type Repo struct {
	// URL is the engine-native location, e.g. "/repos/photos" or
	// "s3:https://acc.r2.cloudflarestorage.com/bucket/photos". It is built from a
	// named destination in Ballast's config, never from a label, so engine-native
	// syntax stays out of the public grammar.
	URL string

	// Password resolves the repository password lazily. It is passed to the child
	// process via a file or environment variable, never on the argv and never
	// logged. For Ballast this derives from the master key (HKDF) unless a
	// per-service secret overrides it.
	Password func() (string, error)

	// Env carries backend credentials (e.g. AWS_ACCESS_KEY_ID) into the child
	// process environment only. Resolved from named secrets, never from labels.
	Env map[string]string
}

// BackupRequest is a single snapshot to write.
type BackupRequest struct {
	Repo Repo

	// Host pins restic's --host to the stable service name so container-id churn
	// never fragments snapshot grouping.
	Host string

	// Tags are applied to the snapshot (Ballast adds its automatic tags on top).
	Tags []string

	// Excludes are glob patterns to skip. ExcludeCaches honors CACHEDIR.TAG.
	Excludes      []string
	ExcludeCaches bool

	// Source: exactly one of Paths or Stdin is set.
	Paths         []string  // filesystem backup
	Stdin         io.Reader // streamed dump (piped from an exec)
	StdinFilename string    // stable filename for a streamed snapshot
}

// BackupResult reports what a backup produced.
type BackupResult struct {
	SnapshotID string
	BytesAdded uint64
	FilesNew   uint64
	Duration   time.Duration

	// BytesProcessed and FilesProcessed are the totals the engine scanned for
	// this snapshot (not just the new/changed bytes in BytesAdded/FilesNew).
	// restic reports them in its summary; the run record surfaces them as its
	// nullable bytes_processed/files_processed. They are zero, not an error,
	// for an engine or a backup that does not report them.
	BytesProcessed uint64
	FilesProcessed uint64
}

// RetentionPolicy mirrors restic's keep-* policy. A zero int means the dimension
// is unset. These names map cleanly onto kopia's keep fields too, keeping the
// interface engine-neutral. A service's labeled policy REPLACES the global one
// wholesale; there is no per-dimension merge.
type RetentionPolicy struct {
	Last     int
	Hourly   int
	Daily    int
	Weekly   int
	Monthly  int
	Yearly   int
	Within   string   // restic duration, e.g. "7d"; empty if unset
	KeepTags []string // snapshots with these tags are never forgotten
}

// Snapshot is one snapshot in a repository.
type Snapshot struct {
	ID       string
	Time     time.Time
	Host     string
	Tags     []string
	Paths    []string
	Hostname string
}

// RestoreRequest restores a snapshot (or the latest, if SnapshotID is "latest")
// to Target.
type RestoreRequest struct {
	Repo       Repo
	SnapshotID string
	Target     string
	Include    []string // restore only these paths, if set
}
