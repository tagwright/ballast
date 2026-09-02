// SPDX-License-Identifier: GPL-3.0-or-later

// Package verify implements `ballast verify`: it restores a snapshot to a
// throwaway location, runs the service's probe, and proves the backup is
// restorable, then emits a ballast.verify.v1 record. It is the GPL heart of the
// Billet product.
//
// It handles a COPY of production data and spawns throwaway containers, so it
// is the most sensitive path in ballast and is built defensively:
//
//   - The live service and its real volumes are never touched. Every restored
//     byte lands in fresh scratch (a scratch directory or fresh named volumes)
//     that this package created.
//   - A throwaway container is ALWAYS placed on core's isolated (internal)
//     network, so a restored copy has no route to production or the internet.
//   - Teardown is reliable on every exit path (success, probe failure, restore
//     failure, timeout, cancellation, or panic): the throwaway container, its
//     scratch volumes, the isolated network, and the scratch directory are all
//     removed, and the record reports whether they were. A leaked scratch copy
//     of production data is recorded as such.
//   - Every throwaway object is labelled so an orphan sweep can find and remove
//     leftovers from a crashed prior run.
//
// The code is engine agnostic: probe, image, restore command, and data_engine
// are free strings supplied by the operator, and mode is the only closed enum.
// Nothing here special-cases a database engine.
package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/record"
	"github.com/tagwright/core/runtime"
)

// cleanupTimeout bounds the detached teardown that runs after the verify
// finishes, times out, or is cancelled, so cleanup never hangs forever.
const cleanupTimeout = 90 * time.Second

// Engine is the subset of the backup engine verify drives: it restores a
// snapshot and lists snapshots to resolve one. Kept small so a fake satisfies
// it in tests. An engine that also reports a version (via a Version method) has
// it recorded as provenance.
type Engine interface {
	Restore(ctx context.Context, req engine.RestoreRequest) error
	Snapshots(ctx context.Context, repo engine.Repo) ([]engine.Snapshot, error)
	Name() string
}

// Deps are the collaborators Run needs.
type Deps struct {
	Runtime runtime.Runtime
	Engine  Engine
	Repo    engine.Repo

	Logger   *slog.Logger
	StateDir string // where the verify record is written; empty disables the write
	HostID   string
	Version  string

	RuntimeName string  // "docker" or "podman", for the record's runtime discriminator
	Trigger     string  // schedule, manual, event, remote
	RequestedBy *string // remote requester identity, nil otherwise

	// TimeoutOverride, when non-nil, replaces spec.Verify.Timeout as the
	// wall-clock bound for this one invocation (and the timeout_ms the record
	// reports). Nil leaves the label value (or its 10m default) in force, so an
	// unset override is byte-for-byte the prior behavior.
	TimeoutOverride *time.Duration

	// NamePrefix prefixes every throwaway object name. Defaults to
	// "ballast-verify". Integration tests set it to "ballast-verify-itest" so
	// their objects are unmistakable and never collide with a real verify.
	NamePrefix string

	// Now is the clock, overridable in tests. Defaults to time.Now.
	Now func() time.Time

	// JSON emits the verify record on Stdout in addition to writing it under
	// StateDir. Stdout defaults to os.Stdout.
	JSON   bool
	Stdout io.Writer
}

func (d Deps) withDefaults() Deps {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.RuntimeName == "" {
		d.RuntimeName = "docker"
	}
	if d.Trigger == "" {
		d.Trigger = "manual"
	}
	if d.NamePrefix == "" {
		d.NamePrefix = "ballast-verify"
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Stdout == nil {
		d.Stdout = os.Stdout
	}
	return d
}

// teardown accumulates cleanup actions and runs them last-in-first-out, so a
// container is always removed before the volumes and network it holds.
type teardown struct {
	actions []func(context.Context) error
}

func (t *teardown) add(f func(context.Context) error) { t.actions = append(t.actions, f) }

func (t *teardown) run(ctx context.Context) error {
	var errs []error
	for i := len(t.actions) - 1; i >= 0; i-- {
		if err := t.actions[i](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// run holds the state of a single verify invocation.
type run struct {
	d         Deps
	spec      *discovery.BackupSpec
	container runtime.Container
	log       *slog.Logger

	v  *record.Verify
	td *teardown

	parentCtx context.Context
	vctx      context.Context // parentCtx bounded by verify.timeout

	timeout    time.Duration // effective wall clock: the label value, or Deps.TimeoutOverride when set
	scratchDir string
}

// Run verifies snapshotReq (an id or "latest") of spec's repository per spec's
// verify configuration, and returns the ballast.verify.v1 record it produced.
// The returned error is non-nil only for a failure to build or persist the
// record itself; the verify's own verdict (pass, fail, inconclusive) lives in
// the record's Result, which the caller maps to an exit status. The container
// argument supplies the service's volume mounts for container mode.
func Run(ctx context.Context, spec *discovery.BackupSpec, container runtime.Container, snapshotReq string, d Deps) (*record.Verify, error) {
	d = d.withDefaults()
	started := d.Now()

	r := &run{
		d:         d,
		spec:      spec,
		container: container,
		log:       d.Logger,
		td:        &teardown{},
		parentCtx: ctx,
	}
	r.timeout = spec.Verify.Timeout
	if d.TimeoutOverride != nil {
		r.timeout = *d.TimeoutOverride
	}
	r.v = r.baseRecord(snapshotReq, started)

	// Sweep leftovers from any crashed prior run before creating our own.
	r.sweepOrphans(ctx)

	vctx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	r.vctx = vctx

	func() {
		defer r.teardownAll()
		defer r.recoverPanic()
		r.dispatch()
	}()

	r.finalize(started)
	err := r.persist()
	return r.v, err
}

// dispatch runs the mechanism spec.Verify.Mode selects.
func (r *run) dispatch() {
	switch r.spec.Verify.Mode {
	case discovery.VerifyModeFiles:
		r.runFiles()
	case discovery.VerifyModeContainer:
		r.runContainer()
	case discovery.VerifyModeStreamRestore:
		r.runStreamRestore()
	default:
		r.inconclusive("other", fmt.Sprintf("unknown verify mode %q", r.spec.Verify.Mode))
	}
}

// recoverPanic converts a panic anywhere in a mode into an inconclusive result
// so a verify never crashes the daemon and its record is still emitted. It is
// deferred before teardownAll, so cleanup still runs after it recovers.
func (r *run) recoverPanic() {
	if p := recover(); p != nil {
		r.log.Error("verify: panic during verify", "service", r.spec.Service, "panic", p)
		r.inconclusive("other", fmt.Sprintf("internal error: %v", p))
	}
}

// teardownAll runs every registered cleanup action on a context detached from
// the verify's own, so cleanup happens even after a timeout or cancellation,
// and records whether the scratch was fully destroyed.
func (r *run) teardownAll() {
	if len(r.td.actions) == 0 {
		r.v.ScratchDestroyed = true
		r.v.ScratchDestroyErr = nil
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.parentCtx), cleanupTimeout)
	defer cancel()
	if err := r.td.run(ctx); err != nil {
		r.log.Error("verify: scratch teardown incomplete; a copy of restored data may have leaked",
			"service", r.spec.Service, "verify_id", r.v.VerifyID, "error", err)
		r.v.ScratchDestroyed = false
		s := err.Error()
		r.v.ScratchDestroyErr = &s
		return
	}
	r.v.ScratchDestroyed = true
	r.v.ScratchDestroyErr = nil
}

// finalize stamps the finishing time and the total duration.
func (r *run) finalize(started time.Time) {
	finished := r.d.Now()
	r.v.FinishedAt = rfc3339(finished)
	r.v.TotalDurationMs = durMs(finished.Sub(started))
}

// persist writes the record under StateDir (when set) and to Stdout (when
// JSON). A write failure is returned so the caller knows the evidence did not
// land, but the record itself is still valid and returned.
func (r *run) persist() error {
	var writeErr error
	if r.d.StateDir != "" {
		if _, err := record.WriteVerify(r.d.StateDir, r.v); err != nil {
			r.log.Warn("verify: write record failed", "service", r.spec.Service, "verify_id", r.v.VerifyID, "error", err)
			writeErr = err
		}
	}
	if r.d.JSON {
		data, err := record.MarshalVerify(r.v)
		if err != nil {
			return errors.Join(writeErr, err)
		}
		if _, err := r.d.Stdout.Write(data); err != nil {
			return errors.Join(writeErr, err)
		}
	}
	return writeErr
}

// baseRecord assembles the static fields common to every mode, with the result
// preset to inconclusive/other so a mode that never sets an outcome (an
// impossible path) still yields a schema-valid non-pass record rather than a
// bare pass.
func (r *run) baseRecord(snapshotReq string, started time.Time) *record.Verify {
	vspec := r.spec.Verify
	v := &record.Verify{
		Record:            record.VerifyRecordType,
		VerifyID:          newULID(),
		HostID:            r.d.HostID,
		Runtime:           r.d.RuntimeName,
		RuntimeRef:        runtimeRef(r.spec),
		Service:           r.spec.Service,
		RepoID:            repoID(r.spec),
		Trigger:           r.d.Trigger,
		RequestedBy:       r.d.RequestedBy,
		SnapshotRequested: firstNonEmpty(snapshotReq, "latest"),
		SnapshotID:        nil,
		SnapshotTime:      nil,
		Mode:              string(vspec.Mode),
		DataEngine:        strptr(vspec.DataEngine),
		Probe:             strptr(vspec.Probe),
		Expect:            strptr(vspec.Expect),
		Assertion:         assertionFor(vspec.Probe, vspec.Expect),
		TimeoutMs:         durMs(r.timeout),
		StartedAt:         rfc3339(started),
		Result:            "inconclusive",
		Reason:            strptr("verify did not run"),
		ReasonCode:        strptr("other"),
		Checked:           map[string]uint64{},
		Dataset:           defaultDataset(r.spec.Service, vspec),
		Restored:          record.Restored{Kind: kindForMode(vspec.Mode), Items: []string{}},
		BallastVersion:    firstNonEmpty(r.d.Version, "unknown"),
		Engine:            record.Engine{Name: r.d.Engine.Name(), Version: engineVersion(r.d.Engine)},
	}
	// image (top level) and environment default to the files-mode shape; the
	// container modes overwrite them when they stand a container up.
	v.Image = nil
	v.Environment = record.Environment{
		Kind:            "scratch-dir",
		Location:        "",
		Image:           nil,
		Network:         nil,
		NetworkIsolated: true,
	}
	return v
}

// --- outcome setters -------------------------------------------------------

func (r *run) pass() {
	r.v.Result = "pass"
	r.v.Reason = nil
	r.v.ReasonCode = nil
}

func (r *run) fail(code, reason string) {
	r.v.Result = "fail"
	r.v.Reason = &reason
	r.v.ReasonCode = &code
}

func (r *run) inconclusive(code, reason string) {
	r.v.Result = "inconclusive"
	r.v.Reason = &reason
	r.v.ReasonCode = &code
}

// ctxReasonCode classifies a context-derived failure during a phase: a parent
// cancellation is always "cancelled"; a verify-timeout expiry maps to the
// phase's timeout code; anything else falls back to fallback.
func (r *run) ctxReasonCode(err error, timeoutCode, fallback string) string {
	if r.parentCtx.Err() != nil && !errors.Is(r.parentCtx.Err(), context.DeadlineExceeded) {
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) || r.vctx.Err() != nil {
		return timeoutCode
	}
	return fallback
}

// --- teardown registration helpers ----------------------------------------

// scratchDirPrefix names every verify's scratch directory, followed by the
// verify id, so the orphan sweep can correlate a leftover scratch with the
// throwaway objects that carry the same id.
const scratchDirPrefix = "ballast-verify-"

// scratchRoot is the parent directory every verify's scratch dir lives under. It
// is deterministic and tied to the state dir so a crashed run's scratch (a copy
// of restored production data) can be found and removed by a later run's orphan
// sweep, rather than lingering under a random OS temp path no sweep knows to
// look at. It falls back to the OS temp dir only when no state dir is configured
// (the degenerate case where the record write is disabled too).
func (r *run) scratchRoot() string {
	if r.d.StateDir != "" {
		return filepath.Join(r.d.StateDir, "verify-scratch")
	}
	return os.TempDir()
}

func (r *run) makeScratchDir() error {
	root := r.scratchRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		return err
	}
	dir := filepath.Join(root, scratchDirPrefix+r.v.VerifyID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	r.scratchDir = dir
	r.td.add(func(context.Context) error {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove scratch dir %s: %w", dir, err)
		}
		return nil
	})
	return nil
}

// sweepOrphans removes throwaway objects a crashed prior run left behind. It is
// conservative so it never disturbs a concurrent live verify: it removes only
// containers that carry our label and are not currently running, and attempts
// to remove labelled networks (RemoveNetwork fails harmlessly on a network a
// live verify still has attached). Named scratch volumes cannot be enumerated
// through the runtime interface, so a hard crash (SIGKILL) can leak one; the
// per-run teardown removes them on every softer exit path.
func (r *run) sweepOrphans(ctx context.Context) {
	prov, ok := r.d.Runtime.(runtime.Provisioner)
	if !ok {
		return
	}
	if conts, err := r.d.Runtime.List(ctx); err == nil {
		for _, c := range conts {
			if _, tagged := c.Labels[labelKey]; !tagged {
				continue
			}
			if c.State == "running" {
				continue
			}
			if err := prov.RemoveContainer(ctx, c.ID, true); err != nil {
				r.log.Debug("verify: orphan container sweep", "container", c.ID, "error", err)
			}
		}
	}
	if insp, ok := r.d.Runtime.(runtime.NetworkInspector); ok {
		if nets, err := insp.ListNetworks(ctx); err == nil {
			for _, n := range nets {
				if _, tagged := n.Labels[labelKey]; !tagged {
					continue
				}
				if err := prov.RemoveNetwork(ctx, n.ID); err != nil {
					r.log.Debug("verify: orphan network sweep", "network", n.ID, "error", err)
				}
			}
		}
	}
	// A hard crash (SIGKILL) leaves the restored dump on disk because no teardown
	// ran. Remove any scratch dir whose verify id no longer has a throwaway object,
	// so a copy of restored production data never outlives the run that made it. A
	// scratch whose throwaway is still present (a live verify, or an orphan still
	// running that a controller's scoped-prefix sweep has yet to force-remove) is
	// preserved, the same running-object correlation the container sweep trusts.
	r.sweepOrphanScratch(ctx)
}

// sweepOrphanScratch removes leftover scratch directories under scratchRoot whose
// verify id has no throwaway container still present. It is the recovery for a
// SIGKILL between makeScratchDir and the deferred teardown: on every softer exit
// teardownAll already removes the scratch.
func (r *run) sweepOrphanScratch(ctx context.Context) {
	root := r.scratchRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return // no scratch root yet, nothing to sweep
	}
	live := map[string]bool{}
	if conts, lerr := r.d.Runtime.List(ctx); lerr == nil {
		for _, c := range conts {
			if id, ok := c.Labels[labelKey]; ok {
				live[id] = true
			}
		}
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), scratchDirPrefix) {
			continue
		}
		vid := strings.TrimPrefix(e.Name(), scratchDirPrefix)
		if live[vid] {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			r.log.Debug("verify: orphan scratch sweep", "dir", e.Name(), "error", err)
		}
	}
}

// --- snapshot resolution ---------------------------------------------------

// resolveSnapshot lists the repository's snapshots and picks the one snapshotReq
// names: the most recent when "latest", or the one whose id has snapshotReq as a
// prefix. It records the resolved id and time on the record. On a listing
// failure it returns runtime/engine-unavailable; on no match it returns
// snapshot_missing. ok is true only when a snapshot was resolved.
func (r *run) resolveSnapshot(snapshotReq string) (engine.Snapshot, bool) {
	snaps, err := r.d.Engine.Snapshots(r.vctx, r.d.Repo)
	if err != nil {
		code := r.ctxReasonCode(err, "other", "runtime_unavailable")
		r.inconclusive(code, fmt.Sprintf("list snapshots: %v", err))
		return engine.Snapshot{}, false
	}
	if len(snaps) == 0 {
		r.inconclusive("snapshot_missing", "the repository has no snapshots to verify")
		return engine.Snapshot{}, false
	}

	var chosen engine.Snapshot
	found := false
	if snapshotReq == "" || snapshotReq == "latest" {
		for _, s := range snaps {
			if !found || s.Time.After(chosen.Time) {
				chosen, found = s, true
			}
		}
	} else {
		for _, s := range snaps {
			if len(s.ID) >= len(snapshotReq) && s.ID[:len(snapshotReq)] == snapshotReq {
				chosen, found = s, true
				break
			}
		}
	}
	if !found {
		r.inconclusive("snapshot_missing", fmt.Sprintf("no snapshot matches %q", snapshotReq))
		return engine.Snapshot{}, false
	}

	id := chosen.ID
	st := rfc3339(chosen.Time)
	r.v.SnapshotID = &id
	r.v.SnapshotTime = &st
	return chosen, true
}

// --- small pure helpers ----------------------------------------------------

// defaultDataset is the provisional human dataset text a record carries from
// the moment it is created, so an early inconclusive (before the restore has
// resolved what exactly it restored) still carries a non-empty dataset. A mode
// refines it once it knows the concrete dump or volume set.
func defaultDataset(service string, v discovery.VerifySpec) string {
	var kind string
	switch v.Mode {
	case discovery.VerifyModeStreamRestore:
		kind = "stream restore"
	case discovery.VerifyModeContainer:
		kind = "container restore"
	default:
		kind = "files"
	}
	if v.DataEngine != "" {
		return fmt.Sprintf("%s (%s %s)", service, v.DataEngine, kind)
	}
	return fmt.Sprintf("%s (%s)", service, kind)
}

func kindForMode(m discovery.VerifyMode) string {
	switch m {
	case discovery.VerifyModeStreamRestore:
		return "stream"
	case discovery.VerifyModeContainer:
		return "volumes"
	default:
		return "paths"
	}
}

// assertionFor names what decides the outcome, consistent with the schema's
// rule that a null probe forces the "manifest" assertion.
func assertionFor(probe, expect string) string {
	if probe == "" {
		return "manifest"
	}
	if expect != "" {
		return "probe_expect"
	}
	return "probe"
}

func runtimeRef(spec *discovery.BackupSpec) map[string]string {
	ref := map[string]string{
		"container_name": spec.ContainerName,
		"container_id":   spec.ContainerID,
	}
	if spec.Project != "" {
		ref["compose_project"] = spec.Project
	}
	return ref
}

func repoID(spec *discovery.BackupSpec) string {
	path := spec.RepoPath
	if path == "" {
		path = spec.Service
	}
	return spec.Destination + ":" + path
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func rfc3339(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05Z") }

// durMs renders a duration as whole non-negative milliseconds for the record.
func durMs(d time.Duration) uint64 {
	if d < 0 {
		return 0
	}
	return uint64(d.Milliseconds())
}

func engineVersion(e Engine) string {
	if v, ok := e.(interface{ Version(context.Context) string }); ok {
		if got := v.Version(context.Background()); got != "" {
			return got
		}
	}
	return "unknown"
}

// sortedVolumeMounts returns the container's volume mounts in a deterministic
// order (by destination), so restored items and dataset text are stable.
func sortedVolumeMounts(c runtime.Container) []runtime.Mount {
	var vols []runtime.Mount
	for _, m := range c.Mounts {
		if m.Type == runtime.MountVolume && m.Source != "" && m.Destination != "" {
			vols = append(vols, m)
		}
	}
	sort.Slice(vols, func(i, j int) bool { return vols[i].Destination < vols[j].Destination })
	return vols
}
