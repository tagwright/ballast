// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/pkg/runtime"
)

// runBackupSteps runs the stop/backup/start portion of the lifecycle
// (RunBackup's steps 4-7), then applies retention (step 8) if the backups
// succeeded. The container, if stopped, is started again before retention
// runs and before this function returns, regardless of whether the backups
// themselves succeeded: the inner closure's defer is scoped tightly around
// stop-and-backup for exactly that reason.
func runBackupSteps(ctx context.Context, spec *discovery.BackupSpec, repo engine.Repo, d Deps, log *slog.Logger) error {
	backupErr := func() error {
		if spec.Stop {
			if err := d.Runtime.Stop(ctx, spec.ContainerID, defaultStopTimeoutSeconds); err != nil {
				return fmt.Errorf("stop container: %w", err)
			}
			defer func() {
				startCtx := context.WithoutCancel(ctx)
				if err := d.Runtime.Start(startCtx, spec.ContainerID); err != nil {
					log.Error("orchestrator: start container after backup", "service", spec.Service, "error", err)
				}
			}()
		}

		var err error
		if len(spec.Paths) > 0 {
			if berr := runFilesystemBackup(ctx, spec, repo, d); berr != nil {
				err = fmt.Errorf("filesystem backup: %w", berr)
			}
		}
		for _, stream := range spec.Streams {
			if serr := runStreamBackup(ctx, spec, stream, repo, d); serr != nil {
				err = combine(err, fmt.Errorf("stream %q backup: %w", stream.ID, serr))
			}
		}
		return err
	}()

	if backupErr != nil {
		return backupErr
	}

	policy := spec.Retention
	if isZeroRetention(policy) {
		p, err := defaultRetentionPolicy(d.Config.Retention)
		if err != nil {
			return fmt.Errorf("parse retention: %w", err)
		}
		policy = p
	}
	if err := d.Engine.Forget(ctx, repo, policy); err != nil {
		return fmt.Errorf("forget: %w", err)
	}
	return nil
}

// autoTags builds the automatic tag set Ballast applies to every snapshot on
// top of the service's own ballast.tags: "ballast", "service=<name>",
// "project=<project>" (only if the service belongs to a compose project),
// and finally kind ("fs" for a filesystem backup, "stream=<id>" for a stream
// dump).
func autoTags(spec *discovery.BackupSpec, kind string) []string {
	tags := make([]string, 0, len(spec.Tags)+4)
	tags = append(tags, spec.Tags...)
	tags = append(tags, "ballast", "service="+spec.Service)
	if spec.Project != "" {
		tags = append(tags, "project="+spec.Project)
	}
	tags = append(tags, kind)
	return tags
}

// runFilesystemBackup writes one snapshot of spec.Paths.
func runFilesystemBackup(ctx context.Context, spec *discovery.BackupSpec, repo engine.Repo, d Deps) error {
	req := engine.BackupRequest{
		Repo:          repo,
		Host:          spec.Service,
		Tags:          autoTags(spec, "fs"),
		Excludes:      spec.Excludes,
		ExcludeCaches: spec.ExcludeCaches,
		Paths:         spec.Paths,
	}
	_, err := d.Engine.Backup(ctx, req)
	return err
}

// runStreamBackup execs stream's dump command in the container and pipes its
// stdout into the engine as a single stdin snapshot.
//
// The exec's stdout is wrapped in a streamWaitReader so that, once it is
// fully drained, the dump's own exit code is checked before the reader
// reports its final EOF to the engine's child process. A non-zero dump exit
// is surfaced as a read error instead of a clean EOF, which makes the
// engine's own backup command fail its stdin copy and abort without
// producing a snapshot: a failed dump must never be recorded as an (empty or
// truncated) snapshot.
func runStreamBackup(ctx context.Context, spec *discovery.BackupSpec, stream discovery.StreamSpec, repo engine.Repo, d Deps) error {
	sctx := ctx
	if stream.Timeout > 0 {
		var cancel context.CancelFunc
		sctx, cancel = context.WithTimeout(ctx, stream.Timeout)
		defer cancel()
	}

	handle, err := d.Runtime.Exec(sctx, spec.ContainerID, runtime.ExecSpec{
		Cmd:  []string{"sh", "-c", stream.Command},
		User: stream.User,
	})
	if err != nil {
		return fmt.Errorf("exec dump: %w", err)
	}

	wait := &streamWaitReader{r: handle.Stdout, wait: handle.Wait}

	req := engine.BackupRequest{
		Repo:          repo,
		Host:          spec.Service,
		Tags:          autoTags(spec, "stream="+stream.ID),
		Excludes:      spec.Excludes,
		ExcludeCaches: spec.ExcludeCaches,
		Stdin:         wait,
		StdinFilename: stream.Filename,
	}
	if _, err := d.Engine.Backup(sctx, req); err != nil {
		return err
	}

	// The engine's child process may have stopped reading (or never started)
	// without driving wait's Read to EOF, e.g. if Backup itself failed
	// before consuming stdin. Whatever the engine did with the data, the
	// dump's own exit code still matters: check it now if nothing has
	// already.
	if !wait.waited {
		if _, err := handle.Wait(); err != nil {
			return fmt.Errorf("dump exited non-zero: %w", err)
		}
	}
	return nil
}

// streamWaitReader wraps a stream dump's stdout. On the first read that
// would report io.EOF, it calls wait (blocking briefly for the exec to
// finish, per runtime.ExecHandle's contract) and, if the dump exited
// non-zero, returns that error instead of io.EOF so the engine's own child
// process sees a broken stdin rather than a clean end of input.
type streamWaitReader struct {
	r    io.Reader
	wait func() (exitCode int, err error)

	waited  bool
	waitErr error
}

func (s *streamWaitReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != io.EOF {
		return n, err
	}
	if !s.waited {
		s.waited = true
		if _, werr := s.wait(); werr != nil {
			s.waitErr = fmt.Errorf("dump exited non-zero: %w", werr)
		}
	}
	if s.waitErr != nil {
		return n, s.waitErr
	}
	return n, io.EOF
}
