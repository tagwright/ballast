// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/manifest"
	"github.com/tagwright/core/runtime"
)

// runBackupSteps runs the stop/backup/start portion of the lifecycle
// (RunBackup's steps 4-7), then applies retention (step 8) if the backups
// succeeded. The container, if stopped, is started again before retention
// runs and before this function returns, regardless of whether the backups
// themselves succeeded: the inner closure's defer is scoped tightly around
// stop-and-backup for exactly that reason.
func runBackupSteps(ctx context.Context, spec *discovery.BackupSpec, repo engine.Repo, d Deps, log *slog.Logger, out *runOutcome) error {
	backupErr := func() error {
		if spec.Stop {
			if err := d.Runtime.Stop(ctx, spec.ContainerID, defaultStopTimeoutSeconds); err != nil {
				return fmt.Errorf("stop container: %w", err)
			}
			out.stopped = true
			defer func() {
				startCtx := context.WithoutCancel(ctx)
				if err := d.Runtime.Start(startCtx, spec.ContainerID); err != nil {
					log.Error("orchestrator: start container after backup", "service", spec.Service, "error", err)
				}
			}()
		}

		var err error
		if len(spec.Paths) > 0 {
			res, berr := runFilesystemBackup(ctx, spec, repo, d)
			if berr != nil {
				err = fmt.Errorf("filesystem backup: %w", berr)
			} else {
				out.fsResult = &res
				// The filesystem pass succeeded and, if stop was requested, the
				// workload is still stopped here (its restart defer has not yet
				// fired), so the manifest is hashed over the same quiesced tree
				// the snapshot captured.
				recordManifest(spec, d, log, out)
			}
		}
		for _, stream := range spec.Streams {
			so, serr := runStreamBackup(ctx, spec, stream, repo, d)
			out.streams = append(out.streams, so)
			if serr != nil {
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
	ferr := d.Engine.Forget(ctx, repo, policy)
	out.retention = &retentionOutcome{applied: ferr == nil, err: ferr}
	if ferr != nil {
		return fmt.Errorf("forget: %w", ferr)
	}
	return nil
}

// recordManifest builds and writes the backup-time manifest for spec, but
// only when the service has verify configured (the opt-in trigger for the
// hash pass), state recording is enabled (d.StateDir set, a run id was
// generated), and there is a filesystem tree to hash. The handle is stored on
// out for the run record.
//
// A manifest failure is logged and swallowed: the backup itself has already
// succeeded, and the manifest is auxiliary compliance evidence, so its
// failure must not turn a good backup into a failed run. In that case the run
// record's manifest stays null, exactly as if verify were not configured.
func recordManifest(spec *discovery.BackupSpec, d Deps, log *slog.Logger, out *runOutcome) {
	if !spec.VerifyConfigured || d.StateDir == "" || out.runID == "" || len(spec.Paths) == 0 {
		return
	}

	location := filepath.Join(d.StateDir, "manifests", spec.Service, out.runID+".json")
	h, err := manifest.Build(spec.Paths, location)
	if err != nil {
		log.Warn("orchestrator: backup-time manifest failed; run record manifest will be null",
			"service", spec.Service, "error", err)
		return
	}
	out.manifest = &h
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

// runFilesystemBackup writes one snapshot of spec.Paths, returning the
// engine's result so the run record can report its bytes and files.
func runFilesystemBackup(ctx context.Context, spec *discovery.BackupSpec, repo engine.Repo, d Deps) (engine.BackupResult, error) {
	req := engine.BackupRequest{
		Repo:          repo,
		Host:          spec.Service,
		Tags:          autoTags(spec, "fs"),
		Excludes:      spec.Excludes,
		ExcludeCaches: spec.ExcludeCaches,
		Paths:         spec.Paths,
	}
	return d.Engine.Backup(ctx, req)
}

// clampExit forces an exit code into the record's 0 to 255 range. A process
// terminated by a signal reports -1 through Go's ExitCode; that and any other
// out-of-range value become 1 (generic failure), so the record never carries
// an exit code the schema rejects.
func clampExit(code int) int {
	if code < 0 {
		return 1
	}
	if code > 255 {
		return 255
	}
	return code
}

// runStreamBackup execs stream's dump command in the container and pipes its
// stdout into the engine as a single stdin snapshot.
//
// The exec's stdout is wrapped in a streamWaitReader so that, once it is
// fully drained, the dump's own exit code is checked before the reader
// reports its final EOF to the engine's child process. A non-zero dump exit
// is surfaced as a read error instead of a clean EOF wherever Go's io.Copy
// loop observes it directly (e.g. if the engine's own backup command fails
// its stdin copy and reports that as its error).
//
// That surfacing is best-effort, not a guarantee: os.Pipe (what os/exec
// wires a generic io.Reader Stdin through) has no way to signal "the writer
// errored" to the reading end, only a plain close, which looks exactly like
// a clean EOF to the child. So restic may already have read a short (even
// empty) stdin to what it sees as a normal end of input, and successfully
// committed a truncated snapshot, before this function ever learns the dump
// failed. To make sure a failed dump never leaves such a snapshot behind
// regardless of that race, the dump's exit is always checked (not only when
// wait hasn't already fired), and if it failed, any snapshot the engine
// reports as written is explicitly deleted before returning the dump error.
func runStreamBackup(ctx context.Context, spec *discovery.BackupSpec, stream discovery.StreamSpec, repo engine.Repo, d Deps) (streamOutcome, error) {
	start := time.Now()
	oc := streamOutcome{id: stream.ID, filename: stream.Filename}

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
		oc.exit = 1
		oc.duration = time.Since(start)
		oc.err = fmt.Errorf("exec dump: %w", err)
		return oc, oc.err
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
	res, backupErr := d.Engine.Backup(sctx, req)

	// The engine's child process may have stopped reading (or never started)
	// without driving wait's Read to EOF, e.g. if Backup itself failed
	// before consuming stdin. Check the dump's own exit unconditionally
	// (not only when wait hasn't already fired) so wait.err below reflects
	// it even when Backup reported success.
	wait.ensureWaited()

	oc.bytes = wait.read
	oc.exit = clampExit(wait.exitCode)

	if dumpErr := wait.err(); dumpErr != nil {
		// The dump failed. Whatever the engine did with the partial (or
		// empty) stdin it received, delete any snapshot it nonetheless
		// wrote: see the doc comment above for why "Backup reported success"
		// does not mean the dump succeeded, and note res.SnapshotID can be
		// populated even when backupErr is also non-nil (engine.Restic.Backup
		// parses a real summary out of stdout regardless of its own error,
		// for exactly this reason).
		if res.SnapshotID != "" {
			delCtx := context.WithoutCancel(ctx)
			if derr := d.Engine.DeleteSnapshot(delCtx, repo, res.SnapshotID); derr != nil {
				oc.err = fmt.Errorf("%w (additionally failed to delete resulting snapshot %s: %v)", dumpErr, res.SnapshotID, derr)
				oc.duration = time.Since(start)
				return oc, oc.err
			}
		}
		oc.err = dumpErr
		oc.duration = time.Since(start)
		return oc, oc.err
	}

	oc.result = res
	oc.produced = res.SnapshotID != ""
	oc.err = backupErr
	oc.duration = time.Since(start)
	return oc, backupErr
}

// streamWaitReader wraps a stream dump's stdout. On the first read that
// would report io.EOF, it calls wait (blocking briefly for the exec to
// finish, per runtime.ExecHandle's contract) and, if the dump exited
// non-zero, returns that error instead of io.EOF so the engine's own child
// process sees a broken stdin rather than a clean end of input.
type streamWaitReader struct {
	r    io.Reader
	wait func() (exitCode int, err error)

	read     uint64
	waited   bool
	exitCode int
	waitErr  error
}

// ensureWaited calls wait exactly once, recording the dump's exit code and, if
// it exited non-zero, the resulting error. It is idempotent, so both the EOF
// path in Read and the unconditional check in runStreamBackup can call it.
func (s *streamWaitReader) ensureWaited() {
	if s.waited {
		return
	}
	s.waited = true
	code, werr := s.wait()
	s.exitCode = code
	if werr != nil {
		s.waitErr = fmt.Errorf("dump exited non-zero: %w", werr)
	}
}

// err returns the dump's non-zero-exit error, if it has been waited on and
// exited non-zero. It is nil if the dump hasn't been waited on yet or exited
// zero.
func (s *streamWaitReader) err() error {
	return s.waitErr
}

func (s *streamWaitReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	s.read += uint64(n)
	if err != io.EOF {
		return n, err
	}
	s.ensureWaited()
	if s.waitErr != nil {
		return n, s.waitErr
	}
	return n, io.EOF
}
