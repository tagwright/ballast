// SPDX-License-Identifier: GPL-3.0-or-later

package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// VerifyRecordType is the discriminator every verify record carries.
const VerifyRecordType = "ballast.verify.v1"

// Verify is one ballast.verify.v1 document. It mirrors the frozen
// ballast.verify.v1 schema field for field; the canonical schema lives in the
// billet-evidence repository, not here, and docs/RECORDS.md points at it.
//
// Every optional value is an explicit null (a pointer, or an always-non-nil map
// or slice), never an absent member, because the contract requires a reader to
// tell not-known from not-reported and the required list is the full member
// list.
type Verify struct {
	Record            string            `json:"record"`
	VerifyID          string            `json:"verify_id"`
	HostID            string            `json:"host_id"`
	Runtime           string            `json:"runtime"`
	RuntimeRef        map[string]string `json:"runtime_ref"`
	Service           string            `json:"service"`
	RepoID            string            `json:"repo_id"`
	Trigger           string            `json:"trigger"`
	RequestedBy       *string           `json:"requested_by"`
	SnapshotRequested string            `json:"snapshot_requested"`
	SnapshotID        *string           `json:"snapshot_id"`
	SnapshotTime      *string           `json:"snapshot_time"`
	Mode              string            `json:"mode"`
	Dataset           string            `json:"dataset"`
	Restored          Restored          `json:"restored"`
	DataEngine        *string           `json:"data_engine"`
	Image             *string           `json:"image"`
	Probe             *string           `json:"probe"`
	Expect            *string           `json:"expect"`
	Assertion         string            `json:"assertion"`
	TimeoutMs         uint64            `json:"timeout_ms"`
	Environment       Environment       `json:"environment"`
	StartedAt         string            `json:"started_at"`
	FinishedAt        string            `json:"finished_at"`
	RestoreDurationMs uint64            `json:"restore_duration_ms"`
	ProbeDurationMs   uint64            `json:"probe_duration_ms"`
	TotalDurationMs   uint64            `json:"total_duration_ms"`
	Result            string            `json:"result"`
	Reason            *string           `json:"reason"`
	ReasonCode        *string           `json:"reason_code"`
	Checked           map[string]uint64 `json:"checked"`
	ManifestCompare   *ManifestCompare  `json:"manifest_compare"`
	ProbeOutput       *CommandOutput    `json:"probe_output"`
	ScratchDestroyed  bool              `json:"scratch_destroyed"`
	ScratchDestroyErr *string           `json:"scratch_destroy_error"`
	BallastVersion    string            `json:"ballast_version"`
	Engine            Engine            `json:"engine"`
}

// Restored is the machine description of what a verify restored: kind is one of
// stream, volumes, or paths, and items names each restored unit.
type Restored struct {
	Kind  string   `json:"kind"`
	Items []string `json:"items"`
}

// Environment records where the restore ran. NetworkIsolated is the DORA
// segregation fact recorded at the source.
type Environment struct {
	Kind            string  `json:"kind"`
	Location        string  `json:"location"`
	Image           *string `json:"image"`
	Network         *string `json:"network"`
	NetworkIsolated bool    `json:"network_isolated"`
}

// ManifestCompare is the files-mode diff of the restored tree against the
// backup-time manifest, null in other modes or when no manifest exists.
type ManifestCompare struct {
	EntriesExpected uint64 `json:"entries_expected"`
	EntriesMatched  uint64 `json:"entries_matched"`
	Mismatched      uint64 `json:"mismatched"`
	Missing         uint64 `json:"missing"`
	Extra           uint64 `json:"extra"`
}

// CommandOutput is a bounded capture of a probe: an excerpt truncated to 4096
// bytes plus a digest of the full untruncated stdout, so the evidence carries
// what the probe actually said without unbounded size. Every member is present,
// null when not captured.
type CommandOutput struct {
	Exit          *int    `json:"exit"`
	StdoutExcerpt *string `json:"stdout_excerpt"`
	StdoutSHA256  *string `json:"stdout_sha256"`
	StderrExcerpt *string `json:"stderr_excerpt"`
}

// MarshalVerify renders v as indented JSON with a trailing newline.
func MarshalVerify(v *Verify) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("record: marshal verify: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteVerify persists v under stateDir/verifies/<service>/<verify_id>.json and
// returns the path it wrote, mirroring the run-record path. Parent directories
// are created as needed.
func WriteVerify(stateDir string, v *Verify) (string, error) {
	dir := filepath.Join(stateDir, "verifies", v.Service)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("record: create dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, v.VerifyID+".json")

	data, err := MarshalVerify(v)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("record: write %q: %w", path, err)
	}
	return path, nil
}
