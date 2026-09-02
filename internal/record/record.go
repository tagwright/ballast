// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package record defines ballast.run.v1, the machine-readable document Ballast
// writes once per backup run. It is a pure data-and-IO package: the
// orchestrator builds a Run from a finished run's facts and hands it here to
// serialize and persist.
//
// The struct mirrors the frozen ballast.run.v1 schema field for field. The
// canonical schema lives in the billet-evidence repository, not here (a
// licensing-placement decision is pending); docs/RECORDS.md describes the
// record in prose and points at it. Every optional value is an explicit null
// (a pointer with no omitempty), never an absent member, because the contract
// requires a reader to tell not-known from not-reported and the required list
// is the full member list.
package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// RecordType is the discriminator every run record carries.
const RecordType = "ballast.run.v1"

// Run is one ballast.run.v1 document.
type Run struct {
	Record         string            `json:"record"`
	RunID          string            `json:"run_id"`
	HostID         string            `json:"host_id"`
	Runtime        string            `json:"runtime"`
	RuntimeRef     map[string]string `json:"runtime_ref"`
	Service        string            `json:"service"`
	RepoID         string            `json:"repo_id"`
	RepoProperties RepoProperties    `json:"repo_properties"`
	Trigger        string            `json:"trigger"`
	RequestedBy    *string           `json:"requested_by"`
	SnapshotID     *string           `json:"snapshot_id"`
	SnapshotTime   *string           `json:"snapshot_time"`
	StartedAt      string            `json:"started_at"`
	FinishedAt     string            `json:"finished_at"`
	DurationMs     uint64            `json:"duration_ms"`
	Paths          []string          `json:"paths"`
	BytesAdded     uint64            `json:"bytes_added"`
	FilesNew       uint64            `json:"files_new"`
	BytesProcessed *uint64           `json:"bytes_processed"`
	FilesProcessed *uint64           `json:"files_processed"`
	Streams        []Stream          `json:"streams"`
	Hooks          Hooks             `json:"hooks"`
	StoppedForBkp  bool              `json:"stopped_for_backup"`
	Retention      *Retention        `json:"retention"`
	Manifest       *Manifest         `json:"manifest"`
	Exit           int               `json:"exit"`
	Error          *string           `json:"error"`
	BallastVersion string            `json:"ballast_version"`
	Engine         Engine            `json:"engine"`
}

// RepoProperties is the repository fact block. Each member is null when the
// property was not assessed, never inferred.
type RepoProperties struct {
	Backend   *string `json:"backend"`
	Offsite   *bool   `json:"offsite"`
	Immutable *bool   `json:"immutable"`
	Encrypted *bool   `json:"encrypted"`
}

// Stream is one stream.<id> outcome.
type Stream struct {
	ID         string  `json:"id"`
	Filename   string  `json:"filename"`
	Bytes      uint64  `json:"bytes"`
	Exit       int     `json:"exit"`
	DurationMs uint64  `json:"duration_ms"`
	Error      *string `json:"error"`
}

// Hooks carries the pre and post consistency-hook outcomes, each null when the
// hook was not declared.
type Hooks struct {
	Pre  *Hook `json:"pre"`
	Post *Hook `json:"post"`
}

// Hook is one exec.pre or exec.post outcome.
type Hook struct {
	Exit       int     `json:"exit"`
	DurationMs uint64  `json:"duration_ms"`
	Error      *string `json:"error"`
}

// Retention is the forget pass outcome, null on the run when it did not run.
type Retention struct {
	Applied          bool    `json:"applied"`
	SnapshotsRemoved uint64  `json:"snapshots_removed"`
	Error            *string `json:"error"`
}

// Manifest is the backup-time manifest handle, null when none was recorded.
type Manifest struct {
	Entries  uint64 `json:"entries"`
	Bytes    uint64 `json:"bytes"`
	Digest   string `json:"digest"`
	Location string `json:"location"`
}

// Engine names the backup engine and its version.
type Engine struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Marshal renders r as indented JSON with a trailing newline.
func Marshal(r *Run) ([]byte, error) {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("record: marshal: %w", err)
	}
	return append(data, '\n'), nil
}

// Write persists r under stateDir/runs/<service>/<run_id>.json and returns the
// path it wrote. Parent directories are created as needed.
func Write(stateDir string, r *Run) (string, error) {
	dir := filepath.Join(stateDir, "runs", r.Service)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("record: create dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, r.RunID+".json")

	data, err := Marshal(r)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("record: write %q: %w", path, err)
	}
	return path, nil
}
