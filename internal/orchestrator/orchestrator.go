// SPDX-License-Identifier: GPL-3.0-or-later

// Package orchestrator runs one service's backup lifecycle end to end: it
// resolves the service's repository, runs the pre-hook, optionally stops the
// container, drives the engine over the filesystem paths and stream dumps
// discovery resolved, restarts the container, applies retention, runs the
// post-hook, and reports the outcome through beacon. It is the integration
// layer that ties runtime, engine, discovery, config, secret, and beacon
// together; it holds no state of its own beyond a single run.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/tagwright/beacon"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/secret"
	"github.com/tagwright/core/runtime"
)

// notifyTimeout bounds how long the final Notify/Report call is allowed to
// take. It runs on a context detached from the run's own ctx, so a run that
// was aborted (or a scheduler shutdown) never suppresses its own outcome
// report.
const notifyTimeout = 30 * time.Second

// defaultStopTimeoutSeconds is how long Runtime.Stop waits for a graceful
// stop before killing the container, for services with ballast.stop=true.
const defaultStopTimeoutSeconds = 30

// Deps are the collaborators RunBackup needs. All fields are required except
// Notifier, which may be nil (outcome reporting is then skipped).
type Deps struct {
	Runtime  runtime.Runtime
	Engine   engine.Engine
	Config   *config.Config
	Resolver secret.Resolver
	Master   []byte
	Notifier *beacon.Beacon
	Logger   *slog.Logger
}

// logger returns d.Logger, falling back to slog.Default() if unset.
func (d Deps) logger() *slog.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return slog.Default()
}

// RunBackup executes spec's full lifecycle:
//
//  1. build the service's engine.Repo from spec and d.Config
//  2. EnsureRepo
//  3. exec.pre (a non-zero exit aborts the run, but exec.post still runs)
//  4. stop the container, if spec.Stop
//  5. filesystem backup, if spec.Paths is non-empty
//  6. stream backups, one per spec.Streams
//  7. start the container again, if it was stopped (before retention)
//  8. retention (forget), spec.Retention or the global default
//  9. exec.post, unconditionally; its own failure only warns
//  10. report the outcome to beacon, both Notify and Report
//
// Every step after the first failure is skipped except exec.post and the
// final outcome report, which always run. The returned error is the first
// failure encountered, if any.
func RunBackup(ctx context.Context, spec *discovery.BackupSpec, d Deps) error {
	start := time.Now()
	log := d.logger()

	var runErr error

	repo, err := BuildRepo(spec, d.Config, d.Resolver, d.Master)
	if err != nil {
		runErr = fmt.Errorf("orchestrator: build repo: %w", err)
	}

	if runErr == nil {
		if err := d.Engine.EnsureRepo(ctx, repo); err != nil {
			runErr = fmt.Errorf("orchestrator: ensure repo: %w", err)
		}
	}

	if runErr == nil && spec.ExecPre != nil {
		if err := runHook(ctx, d.Runtime, spec.ContainerID, spec.ExecPre); err != nil {
			runErr = fmt.Errorf("orchestrator: pre-hook: %w", err)
		}
	}

	if runErr == nil {
		runErr = runBackupSteps(ctx, spec, repo, d, log)
	}

	if spec.ExecPost != nil {
		if err := runHook(ctx, d.Runtime, spec.ContainerID, spec.ExecPost); err != nil {
			log.Warn("orchestrator: post-hook failed", "service", spec.Service, "error", err)
		}
	}

	reportOutcome(ctx, d, spec, runErr, time.Since(start), log)

	return runErr
}

// BuildRepo resolves spec's destination into an engine.Repo: the URL joins
// the named destination's URL with spec.RepoPath, the password closure
// derives from master unless spec.PasswordSecret overrides it, and Env
// resolves every one of the destination's named secrets up front.
//
// Exported so the daemon's maintenance jobs (prune, check) can build the
// same Repo for a service outside of a full RunBackup.
func BuildRepo(spec *discovery.BackupSpec, cfg *config.Config, resolver secret.Resolver, master []byte) (engine.Repo, error) {
	dest, ok := cfg.Destinations[spec.Destination]
	if !ok {
		return engine.Repo{}, fmt.Errorf("orchestrator: unknown destination %q", spec.Destination)
	}

	env := make(map[string]string, len(dest.Env))
	for envVar, secretName := range dest.Env {
		v, err := resolver(secretName)
		if err != nil {
			return engine.Repo{}, fmt.Errorf("orchestrator: resolve destination secret %q (for %s): %w", secretName, envVar, err)
		}
		env[envVar] = v
	}

	passwordSecret := spec.PasswordSecret
	service := spec.Service
	password := func() (string, error) {
		if passwordSecret != "" {
			return resolver(passwordSecret)
		}
		return secret.DeriveRepoPassword(master, service)
	}

	return engine.Repo{
		URL:      joinRepoURL(dest.URL, spec.RepoPath),
		Password: password,
		Env:      env,
	}, nil
}

// joinRepoURL appends sub as a path segment of base. base carries
// engine-native syntax (a local path or a backend URL like
// "s3:https://host/bucket"); sub is always a plain relative path segment, so
// a simple slash join is sufficient.
func joinRepoURL(base, sub string) string {
	if sub == "" {
		return base
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(sub, "/")
}

// runHook execs hook's command in the container, honoring its Timeout via a
// derived context and its User via ExecSpec.User. The command is run through
// "sh -c" since HookSpec.Command is a single shell command line, not an
// argv. Stdout is drained and discarded; a non-zero exit is returned as an
// error.
func runHook(ctx context.Context, rt runtime.Runtime, containerID string, hook *discovery.HookSpec) error {
	hctx := ctx
	if hook.Timeout > 0 {
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, hook.Timeout)
		defer cancel()
	}

	handle, err := rt.Exec(hctx, containerID, runtime.ExecSpec{
		Cmd:  []string{"sh", "-c", hook.Command},
		User: hook.User,
	})
	if err != nil {
		return fmt.Errorf("exec: %w", err)
	}

	if _, err := io.Copy(io.Discard, handle.Stdout); err != nil {
		return fmt.Errorf("drain output: %w", err)
	}
	if _, err := handle.Wait(); err != nil {
		return fmt.Errorf("non-zero exit: %w", err)
	}
	return nil
}

// reportOutcome builds a beacon.Notification and beacon.Health from runErr
// and hands both to d.Notifier. It runs on a context detached from ctx (with
// its own bounded timeout) so a cancelled run context never suppresses the
// outcome report, and it tolerates a nil Notifier by doing nothing.
//
// spec.NotifySuppress mutes the Notify call only: the Report call (telemetry
// / health push) always runs regardless, since it is not an alert channel.
// On success, spec.NotifyOnSuccess raises the notification from LevelInfo to
// LevelWarning so it surfaces on warn-and-above channels; failures always
// notify at LevelError either way.
func reportOutcome(ctx context.Context, d Deps, spec *discovery.BackupSpec, runErr error, elapsed time.Duration, log *slog.Logger) {
	if d.Notifier == nil {
		return
	}

	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()

	ok := runErr == nil
	fields := map[string]string{
		"service":  spec.Service,
		"duration": elapsed.Round(time.Second).String(),
	}

	successLevel := beacon.LevelInfo
	if spec.NotifyOnSuccess {
		successLevel = beacon.LevelWarning
	}

	n := beacon.Notification{
		Title:  fmt.Sprintf("Backup OK: %s", spec.Service),
		Body:   fmt.Sprintf("Backup completed for %s in %s.", spec.Service, elapsed.Round(time.Second)),
		Level:  successLevel,
		Fields: fields,
	}
	message := ""
	if !ok {
		n.Title = fmt.Sprintf("Backup FAILED: %s", spec.Service)
		n.Body = fmt.Sprintf("Backup for %s failed after %s: %v", spec.Service, elapsed.Round(time.Second), runErr)
		n.Level = beacon.LevelError
		message = runErr.Error()
	}

	if !spec.NotifySuppress {
		if err := d.Notifier.Notify(nctx, n); err != nil {
			log.Warn("orchestrator: notify failed", "service", spec.Service, "error", err)
		}
	}

	h := beacon.Health{
		Name:     spec.Service,
		OK:       ok,
		Message:  message,
		Duration: elapsed,
	}
	if err := d.Notifier.Report(nctx, h); err != nil {
		log.Warn("orchestrator: telemetry report failed", "service", spec.Service, "error", err)
	}
}

// combine joins two errors, dropping whichever is nil, so callers that
// accumulate errors across an unbounded number of steps (streams) don't have
// to special-case the first one.
func combine(a, b error) error {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	return errors.Join(a, b)
}
