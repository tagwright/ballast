// SPDX-License-Identifier: GPL-3.0-or-later

// Package manifest records a backup-time manifest of a filesystem tree: one
// entry per regular file with its path, size, and SHA-256. A files-mode
// verify later restores the snapshot and diffs the restored tree against this
// manifest, so the manifest is the ground truth a restore is checked against.
//
// Building a manifest costs a full hash pass over the backed-up bytes. It is
// therefore opt-in: the orchestrator builds one only for a service that has
// verify configured, never as a matter of course. When a service has no
// verify configuration the run record's manifest handle is null and no hash
// pass runs at all.
//
// The on-disk manifest format here is Ballast's own and is not one of the
// frozen contracts. Only the handle the run record embeds (entries, bytes,
// digest, location) is contractual, via ballast.run.v1.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Record identifies the on-disk manifest format version.
const Record = "ballast.manifest.v1"

// Handle is the compact summary of a written manifest that the run record
// embeds. Its fields match ballast.run.v1's manifest object exactly.
type Handle struct {
	Entries  uint64
	Bytes    uint64
	Digest   string // "sha256:" plus 64 lowercase hex over the manifest file bytes
	Location string
}

// Entry is one file in the manifest.
type Entry struct {
	Path   string `json:"path"`
	Size   uint64 `json:"size"`
	SHA256 string `json:"sha256"` // "sha256:" plus 64 lowercase hex over the file contents
}

// document is the serialized manifest, written to Handle.Location.
type document struct {
	Record  string  `json:"record"`
	Entries []Entry `json:"entries"`
}

// Build walks each root in roots, hashing every regular file it finds, writes
// the manifest as JSON to location (creating parent directories), and returns
// the handle the run record embeds.
//
// Entries are sorted by path, so the manifest bytes (and therefore the
// digest) are deterministic for a given tree regardless of directory
// iteration order. Symlinks and non-regular files are skipped: only regular
// file contents are what a restore reproduces and a files-mode verify checks.
func Build(roots []string, location string) (Handle, error) {
	var entries []Entry
	var totalBytes uint64

	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.Type().IsRegular() {
				return nil
			}
			sum, size, herr := hashFile(path)
			if herr != nil {
				return herr
			}
			entries = append(entries, Entry{
				Path:   path,
				Size:   size,
				SHA256: "sha256:" + sum,
			})
			totalBytes += size
			return nil
		})
		if err != nil {
			return Handle{}, fmt.Errorf("manifest: walk %q: %w", root, err)
		}
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })

	body, err := json.Marshal(document{Record: Record, Entries: entries})
	if err != nil {
		return Handle{}, fmt.Errorf("manifest: marshal: %w", err)
	}

	digest := sha256.Sum256(body)

	if err := os.MkdirAll(filepath.Dir(location), 0o755); err != nil {
		return Handle{}, fmt.Errorf("manifest: create dir for %q: %w", location, err)
	}
	if err := os.WriteFile(location, body, 0o644); err != nil {
		return Handle{}, fmt.Errorf("manifest: write %q: %w", location, err)
	}

	return Handle{
		Entries:  uint64(len(entries)),
		Bytes:    totalBytes,
		Digest:   "sha256:" + hex.EncodeToString(digest[:]),
		Location: location,
	}, nil
}

// hashFile returns the lowercase hex SHA-256 of path's contents and its size
// in bytes.
func hashFile(path string) (string, uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()

	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), uint64(n), nil
}
