// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package record

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// CheckRecordType is the discriminator every check record carries.
const CheckRecordType = "ballast.check.v1"

// Check method values. This distinction is the whole point of the record and
// must never be lost or blurred: it is what separates a strong claim from a
// weak one, and a compliance report that treats the two as the same overclaims.
//
//   - CheckMethodMetadata is `restic check`: it walks the repository's
//     structure and index and confirms every referenced pack and blob is
//     present and internally consistent. It does NOT read the pack data, so it
//     proves nothing about whether the stored bytes are intact or whether
//     anything actually restores. It is fast and cheap and worth running often,
//     but it is the weaker claim.
//   - CheckMethodReadData is `restic check --read-data`: it additionally reads
//     every pack and re-hashes its data, so it detects bit rot and silent
//     backend corruption the metadata pass cannot. It is expensive (it reads
//     the whole repository) and is the stronger integrity claim.
//
// Neither method is a restore test. An integrity check proves the repository is
// internally consistent (and, with read-data, that its bytes still hash);
// `ballast verify` is the separate evidence that a snapshot actually restores
// and runs. Downstream these are SEPARATE evidence: a check record must never
// be folded into or counted as verify evidence, and a metadata check must never
// be presented as if it were a data read.
const (
	// CheckMethodMetadata is `restic check`: structure, index, and metadata
	// only, NO data blobs read.
	CheckMethodMetadata = "metadata"
	// CheckMethodReadData is `restic check --read-data`: every pack's data read
	// and re-hashed.
	CheckMethodReadData = "read-data"
)

// Check is one ballast.check.v1 document. It records the outcome of a
// repository integrity check (`restic check`, with or without --read-data). It
// mirrors the frozen ballast.check.v1 schema field for field; the canonical
// schema lives in the billet-evidence repository, not here, and
// docs/RECORDS.md points at it.
//
// Every optional value is an explicit null (a pointer, or an always-non-nil
// map), never an absent member, because the contract requires a reader to tell
// not-known from not-reported and the required list is the full member list.
type Check struct {
	Record         string            `json:"record"`      // const CheckRecordType
	CheckID        string            `json:"check_id"`    // ULID
	HostID         string            `json:"host_id"`
	Runtime        string            `json:"runtime"`
	RuntimeRef     map[string]string `json:"runtime_ref"`
	Service        string            `json:"service"`
	RepoID         string            `json:"repo_id"`
	Trigger        string            `json:"trigger"`      // schedule | manual | remote
	RequestedBy    *string           `json:"requested_by"` // null unless remote
	Method         string            `json:"method"`       // CheckMethodMetadata | CheckMethodReadData
	StartedAt      string            `json:"started_at"`   // RFC3339 UTC
	FinishedAt     string            `json:"finished_at"`
	DurationMs     uint64            `json:"duration_ms"`
	Result         string            `json:"result"`      // pass | fail | inconclusive
	Reason         *string           `json:"reason"`      // null on pass; bounded restic error text on fail
	ReasonCode     *string           `json:"reason_code"` // null on pass; check_errors | repo_unreachable | cancelled | other
	BallastVersion string            `json:"ballast_version"`
	Engine         Engine            `json:"engine"`
}

// MarshalCheck renders c as indented JSON with a trailing newline.
func MarshalCheck(c *Check) ([]byte, error) {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("record: marshal check: %w", err)
	}
	return append(data, '\n'), nil
}

// WriteCheck persists c under stateDir/checks/<service>/<check_id>.json and
// returns the path it wrote, mirroring the run- and verify-record paths. Parent
// directories are created as needed.
func WriteCheck(stateDir string, c *Check) (string, error) {
	dir := filepath.Join(stateDir, "checks", c.Service)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("record: create dir %q: %w", dir, err)
	}
	path := filepath.Join(dir, c.CheckID+".json")

	data, err := MarshalCheck(c)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("record: write %q: %w", path, err)
	}
	return path, nil
}
