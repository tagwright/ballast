// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package check runs a repository integrity check (`restic check`, with or
// without --read-data) and builds the ballast.check.v1 record for it. It is the
// one place the check-outcome mapping lives, so the CLI (`ballast check`) and
// the daemon's scheduled maintenance job produce byte-identical records from
// the same facts.
//
// An integrity check is NOT a restore test. A metadata check proves the
// repository is internally consistent; a read-data check additionally proves
// its bytes still hash; neither proves anything restores. That is what
// `ballast verify` is for, and its evidence is kept separate downstream. The
// method field on the record carries which of the two claims this check makes.
package check

import (
	"context"
	"errors"
	"time"

	"github.com/tagwright/ballast/internal/discovery"
	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/record"
	"github.com/tagwright/ballast/internal/ulid"
)

// reasonMaxBytes bounds the restic error text stored on a fail record, matching
// the 4096-byte cap the verify record uses for captured output.
const reasonMaxBytes = 4096

// Engine is the subset of the backup engine a check drives: it checks a
// repository and names itself. An engine that also reports a version (via a
// Version method) has it recorded as provenance.
type Engine interface {
	Check(ctx context.Context, repo engine.Repo, readData bool) error
	Name() string
}

// Params carries everything building a check record needs beyond the engine
// result itself. RuntimeName is the runtime discriminator ("docker" or
// "podman"); Trigger is "schedule", "manual", or "remote"; RequestedBy is the
// remote requester identity (nil otherwise); ReadData selects the method.
type Params struct {
	Spec        *discovery.BackupSpec
	HostID      string
	RuntimeName string
	Version     string
	Trigger     string
	RequestedBy *string
	ReadData    bool

	// Now is the clock, overridable in tests. Defaults to time.Now.
	Now func() time.Time
}

// Run executes eng.Check against repo per p.ReadData and returns the built
// ballast.check.v1 record. The returned record's Result carries the verdict
// (pass, fail, inconclusive); Run itself does not error on a failing check, the
// same way verify returns its verdict in the record rather than as an error.
func Run(ctx context.Context, eng Engine, repo engine.Repo, p Params) *record.Check {
	now := p.Now
	if now == nil {
		now = time.Now
	}

	started := now()
	err := eng.Check(ctx, repo, p.ReadData)
	finished := now()

	return build(p, err, started, finished, eng)
}

// build maps a check outcome onto a record. It is the honest outcome mapping,
// factored out so it is unit-testable without a live restic:
//
//   - no error            -> pass, reason and reason_code null
//   - context.Canceled    -> inconclusive, reason_code "cancelled"
//   - any other error      -> fail, reason_code "check_errors", bounded reason
func build(p Params, checkErr error, started, finished time.Time, eng Engine) *record.Check {
	c := &record.Check{
		Record:         record.CheckRecordType,
		CheckID:        newULID(),
		HostID:         p.HostID,
		Runtime:        firstNonEmpty(p.RuntimeName, "docker"),
		RuntimeRef:     runtimeRef(p.Spec),
		Service:        p.Spec.Service,
		RepoID:         repoID(p.Spec),
		Trigger:        firstNonEmpty(p.Trigger, "manual"),
		RequestedBy:    p.RequestedBy,
		Method:         methodFor(p.ReadData),
		StartedAt:      rfc3339(started),
		FinishedAt:     rfc3339(finished),
		DurationMs:     durMs(finished.Sub(started)),
		BallastVersion: firstNonEmpty(p.Version, "unknown"),
		Engine:         record.Engine{Name: eng.Name(), Version: engineVersion(eng)},
	}

	switch {
	case checkErr == nil:
		c.Result = "pass"
		c.Reason = nil
		c.ReasonCode = nil
	case errors.Is(checkErr, context.Canceled):
		// The check was cancelled (parent context) before it could reach a
		// verdict: the repository's integrity is not known, not proven bad.
		c.Result = "inconclusive"
		setReason(c, "cancelled", checkErr.Error())
	default:
		// restic returned errors: the repository failed its integrity check.
		c.Result = "fail"
		setReason(c, "check_errors", checkErr.Error())
	}
	return c
}

// setReason stamps a non-pass reason_code and its bounded human reason on the
// record, keeping the pointer-and-bound handling in one place.
func setReason(c *record.Check, code, reason string) {
	b := bounded(reason)
	c.Reason = &b
	c.ReasonCode = &code
}

// methodFor names the check method, the record's must-not-be-lost distinction
// between a metadata-only check and a full data read.
func methodFor(readData bool) string {
	if readData {
		return record.CheckMethodReadData
	}
	return record.CheckMethodMetadata
}

// --- small pure helpers ----------------------------------------------------

// newULID returns a fresh ULID for a check_id. On the impossible CSPRNG-failure
// path a fixed sentinel keeps the record schema-valid (26 Crockford characters)
// rather than producing an empty id, matching the verify path.
func newULID() string {
	id, err := ulid.New()
	if err != nil {
		return "00000000000000000000000000"
	}
	return id
}

// runtimeRef builds the open runtime locator map: container name and id always,
// compose project only when the service belongs to one.
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

// engineVersion reads the engine's version via an optional capability, so the
// Engine interface needs no version method; an engine that does not expose one
// records "unknown", the non-empty provenance string the record permits.
func engineVersion(e Engine) string {
	if v, ok := e.(interface{ Version(context.Context) string }); ok {
		if got := v.Version(context.Background()); got != "" {
			return got
		}
	}
	return "unknown"
}

// bounded truncates s to at most reasonMaxBytes bytes, trimming any partial
// UTF-8 sequence left at the cut so the stored reason is always valid UTF-8. A
// truncation marker is appended when anything was dropped.
func bounded(s string) string {
	if len(s) <= reasonMaxBytes {
		return s
	}
	const marker = "...[truncated]"
	limit := reasonMaxBytes - len(marker)
	// Back off the cut to a UTF-8 boundary: bytes 0x80-0xBF are continuation
	// bytes, so step left until the next byte starts a new rune.
	for limit > 0 && s[limit]&0xC0 == 0x80 {
		limit--
	}
	return s[:limit] + marker
}
