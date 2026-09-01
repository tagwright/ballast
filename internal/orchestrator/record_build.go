// SPDX-License-Identifier: GPL-3.0-or-later

package orchestrator

import (
	"context"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/record"
)

// emitRunRecord builds the ballast.run.v1 record for a finished run and writes
// it under d.StateDir (when set) and to d.Stdout (when d.JSON). It is a no-op
// unless recording is activated by the caller, which is what keeps a bare
// RunBackup unchanged. Any failure to build, write, or emit the record is
// logged and swallowed: the record is evidence about the run, and failing to
// write it must never change the run's own success or failure.
func emitRunRecord(spec *discovery.BackupSpec, d Deps, log *slog.Logger, out *runOutcome, runErr error, start, finished time.Time) {
	if out.runID == "" || (d.StateDir == "" && !d.JSON) {
		return
	}

	r := buildRunRecord(spec, d, out, runErr, start, finished)

	if d.StateDir != "" {
		if _, err := record.Write(d.StateDir, r); err != nil {
			log.Warn("orchestrator: write run record failed", "service", spec.Service, "run_id", out.runID, "error", err)
		}
	}
	if d.JSON {
		w := d.Stdout
		if w == nil {
			w = os.Stdout
		}
		data, err := record.Marshal(r)
		if err != nil {
			log.Warn("orchestrator: marshal run record failed", "service", spec.Service, "run_id", out.runID, "error", err)
		} else if _, err := w.Write(data); err != nil {
			log.Warn("orchestrator: emit run record failed", "service", spec.Service, "run_id", out.runID, "error", err)
		}
	}
}

// buildRunRecord assembles the ballast.run.v1 document from a finished run's
// facts.
func buildRunRecord(spec *discovery.BackupSpec, d Deps, out *runOutcome, runErr error, start, finished time.Time) *record.Run {
	snapshotID := resolveSnapshotID(out)
	bytesAdded, filesNew, bytesProc, filesProc := aggregateBytes(out)

	r := &record.Run{
		Record:         record.RecordType,
		RunID:          out.runID,
		HostID:         d.HostID,
		Runtime:        firstNonEmpty(d.Config.Runtime, "docker"),
		RuntimeRef:     runtimeRef(spec),
		Service:        spec.Service,
		RepoID:         repoID(spec),
		RepoProperties: repoProperties(d.Config, spec.Destination),
		Trigger:        firstNonEmpty(d.Trigger, "manual"),
		RequestedBy:    d.RequestedBy,
		StartedAt:      rfc3339(start),
		FinishedAt:     rfc3339(finished),
		DurationMs:     uint64(finished.Sub(start).Milliseconds()),
		Paths:          nonNilStrings(spec.Paths),
		BytesAdded:     bytesAdded,
		FilesNew:       filesNew,
		Streams:        streamRecords(out),
		Hooks:          hookRecords(out),
		StoppedForBkp:  out.stopped,
		Retention:      retentionRecord(out),
		Manifest:       manifestRecord(out),
		BallastVersion: firstNonEmpty(d.Version, "unknown"),
		Engine:         engineRecord(d),
	}

	if snapshotID != "" {
		st := rfc3339(start)
		r.SnapshotID = &snapshotID
		r.SnapshotTime = &st
		r.BytesProcessed = &bytesProc
		r.FilesProcessed = &filesProc
	}

	switch {
	case runErr != nil:
		r.Exit = 1
		s := runErr.Error()
		r.Error = &s
	case snapshotID == "":
		// The run reported no error yet produced no snapshot (a service with
		// no paths and no streams). There is no backup to attest, so the
		// record reflects that as a non-success rather than claim a passing
		// backup with a null snapshot, which the schema forbids under exit 0.
		r.Exit = 1
		s := "orchestrator: run produced no snapshot"
		r.Error = &s
	default:
		r.Exit = 0
	}

	return r
}

// resolveSnapshotID picks the run's representative snapshot: the filesystem
// pass's snapshot if one was produced, otherwise the last stream snapshot that
// was written and kept.
func resolveSnapshotID(out *runOutcome) string {
	if out.fsResult != nil && out.fsResult.SnapshotID != "" {
		return out.fsResult.SnapshotID
	}
	id := ""
	for _, s := range out.streams {
		if s.produced && s.result.SnapshotID != "" {
			id = s.result.SnapshotID
		}
	}
	return id
}

// aggregateBytes sums the engine's byte and file accounting across the
// filesystem pass and every stream that produced a snapshot.
func aggregateBytes(out *runOutcome) (bytesAdded, filesNew, bytesProc, filesProc uint64) {
	if out.fsResult != nil {
		bytesAdded += out.fsResult.BytesAdded
		filesNew += out.fsResult.FilesNew
		bytesProc += out.fsResult.BytesProcessed
		filesProc += out.fsResult.FilesProcessed
	}
	for _, s := range out.streams {
		if s.produced {
			bytesAdded += s.result.BytesAdded
			filesNew += s.result.FilesNew
			bytesProc += s.result.BytesProcessed
			filesProc += s.result.FilesProcessed
		}
	}
	return
}

// runtimeRef builds the open runtime locator map: container name and id
// always, compose project only when the service belongs to one.
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

// repoID is the destination name, a colon, then the repository path within it.
func repoID(spec *discovery.BackupSpec) string {
	path := spec.RepoPath
	if path == "" {
		path = spec.Service
	}
	return spec.Destination + ":" + path
}

// backendToken matches a restic backend scheme prefix (lowercase, e.g. "s3",
// "b2", "sftp", "rest", "azure", "gs").
var backendToken = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// repoProperties reports the facts Ballast can assess about the destination
// repository, and null for the rest. encrypted is true as a fact of the restic
// engine (restic always encrypts). backend is read from the destination URL's
// scheme. offsite and immutable are never inferred: Ballast cannot tell from a
// URL whether a backend is physically offsite or has object lock, so they stay
// null (not assessed) for a profile to treat as unknown rather than assume.
func repoProperties(cfg *config.Config, destName string) record.RepoProperties {
	encrypted := true
	props := record.RepoProperties{Encrypted: &encrypted}

	if cfg != nil {
		if dest, ok := cfg.Destinations[destName]; ok && dest.URL != "" {
			backend := backendOf(dest.URL)
			props.Backend = &backend
		}
	}
	return props
}

// backendOf derives a backend name from a restic repository URL: the scheme
// before the first colon when that scheme is a backend token, otherwise
// "local" for a bare filesystem path.
func backendOf(url string) string {
	idx := strings.IndexByte(url, ':')
	if idx <= 0 {
		return "local"
	}
	scheme := url[:idx]
	if backendToken.MatchString(scheme) {
		return scheme
	}
	return "local"
}

// streamRecords maps the accumulated stream outcomes onto record.Stream,
// always returning a non-nil slice so the record carries an empty array rather
// than a null.
func streamRecords(out *runOutcome) []record.Stream {
	streams := make([]record.Stream, 0, len(out.streams))
	for _, s := range out.streams {
		streams = append(streams, record.Stream{
			ID:         s.id,
			Filename:   s.filename,
			Bytes:      s.bytes,
			Exit:       s.exit,
			DurationMs: uint64(s.duration.Milliseconds()),
			Error:      errString(s.err),
		})
	}
	return streams
}

// hookRecords maps the pre and post hook outcomes onto the record, leaving a
// hook null when it was not declared.
func hookRecords(out *runOutcome) record.Hooks {
	return record.Hooks{
		Pre:  hookRecord(out.pre),
		Post: hookRecord(out.post),
	}
}

func hookRecord(oc *hookOutcome) *record.Hook {
	if oc == nil {
		return nil
	}
	return &record.Hook{
		Exit:       oc.exit,
		DurationMs: uint64(oc.duration.Milliseconds()),
		Error:      errString(oc.err),
	}
}

// retentionRecord maps the forget outcome onto the record, null when forget
// did not run. snapshots_removed is reported as zero: Ballast does not yet
// surface the count from the engine, and the field is informational, not
// consumed by any coverage rule.
func retentionRecord(out *runOutcome) *record.Retention {
	if out.retention == nil {
		return nil
	}
	return &record.Retention{
		Applied:          out.retention.applied,
		SnapshotsRemoved: 0,
		Error:            errString(out.retention.err),
	}
}

// manifestRecord maps the manifest handle onto the record, null when none was
// recorded.
func manifestRecord(out *runOutcome) *record.Manifest {
	if out.manifest == nil {
		return nil
	}
	return &record.Manifest{
		Entries:  out.manifest.Entries,
		Bytes:    out.manifest.Bytes,
		Digest:   out.manifest.Digest,
		Location: out.manifest.Location,
	}
}

// engineRecord names the engine and its version. The version is looked up via
// an optional capability so the engine interface needs no version method: an
// engine that does not expose one records "unknown", which the record permits
// as a non-empty provenance string.
func engineRecord(d Deps) record.Engine {
	version := "unknown"
	if v, ok := d.Engine.(interface {
		Version(context.Context) string
	}); ok {
		if got := v.Version(context.Background()); got != "" {
			version = got
		}
	}
	return record.Engine{Name: d.Engine.Name(), Version: version}
}

// firstNonEmpty returns a if it is non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// rfc3339 renders t as an RFC 3339 UTC timestamp with a literal Z and no
// fractional seconds, the one spelling the contracts accept.
func rfc3339(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// nonNilStrings returns s, or an empty (non-nil) slice when s is nil, so the
// record marshals an empty array rather than a null.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// errString returns a pointer to err's message, or nil for a nil error, so the
// record carries either a string or an explicit null.
func errString(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}
