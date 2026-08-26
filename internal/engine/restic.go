// SPDX-License-Identifier: GPL-3.0-or-later

package engine

import "context"

// Restic drives the restic CLI. It is the first and default engine.
//
// The method bodies are stubs: the interface, the request and result types, and
// the one-repo-per-service contract are settled, and the CLI invocations (with
// --json parsing, RESTIC_PASSWORD_FILE for the password, and stdin piping for
// streamed dumps) land in the next pass.
type Restic struct {
	// binary is the path to the restic executable.
	binary string
}

// NewRestic returns a restic driver using the given binary path (default
// "restic" if empty).
func NewRestic(binary string) *Restic {
	if binary == "" {
		binary = "restic"
	}
	return &Restic{binary: binary}
}

// compile-time assertion that the driver satisfies the interface.
var _ Engine = (*Restic)(nil)

func (r *Restic) Name() string { return "restic" }

func (r *Restic) EnsureRepo(ctx context.Context, repo Repo) error {
	return ErrNotImplemented
}

func (r *Restic) Backup(ctx context.Context, req BackupRequest) (BackupResult, error) {
	return BackupResult{}, ErrNotImplemented
}

func (r *Restic) Forget(ctx context.Context, repo Repo, policy RetentionPolicy) error {
	return ErrNotImplemented
}

func (r *Restic) Prune(ctx context.Context, repo Repo) error {
	return ErrNotImplemented
}

func (r *Restic) Check(ctx context.Context, repo Repo, readData bool) error {
	return ErrNotImplemented
}

func (r *Restic) Snapshots(ctx context.Context, repo Repo) ([]Snapshot, error) {
	return nil, ErrNotImplemented
}

func (r *Restic) Restore(ctx context.Context, req RestoreRequest) error {
	return ErrNotImplemented
}
