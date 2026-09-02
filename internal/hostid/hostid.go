// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

// Package hostid owns Ballast's stable host identity: the "h_" identifier
// generated once from a CSPRNG, persisted in the state directory, and loaded
// unchanged on every subsequent start.
//
// The identity is the fleet plane's join key (Billet's records key their host
// on it), so it must survive container recreation and must never silently
// change: a corrupt or unreadable host_id file is surfaced as an error rather
// than papered over with a fresh identity that would orphan every record
// already written under the old one.
//
// The format is frozen by the ballast.run.v1 contract: the ASCII string "h_"
// followed by 4 to 64 lowercase hex characters.
package hostid

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// FileName is the basename of the host-identity file inside the state
// directory.
const FileName = "host_id"

// generatedBytes is how many CSPRNG bytes a freshly generated identity draws.
// Eight bytes render as 16 lowercase hex characters, comfortably inside the
// frozen 4-to-64 range and matching the width the schema's examples use.
const generatedBytes = 8

// pattern is the frozen host_id shape from the ballast.run.v1 contract.
var pattern = regexp.MustCompile(`^h_[0-9a-f]{4,64}$`)

// Valid reports whether id matches the frozen host_id format.
func Valid(id string) bool {
	return pattern.MatchString(id)
}

// Generate returns a fresh host identity: "h_" followed by hex of
// generatedBytes CSPRNG bytes. It panics only if the system CSPRNG fails,
// which is the same unrecoverable condition crypto/rand.Read signals
// everywhere else it is used.
func Generate() (string, error) {
	buf := make([]byte, generatedBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("hostid: read CSPRNG: %w", err)
	}
	return "h_" + hex.EncodeToString(buf), nil
}

// LoadOrCreate returns the host identity stored in stateDir, generating and
// persisting one on first run only.
//
// An existing file is read and validated: a value that does not match the
// frozen format is an error, never a trigger to regenerate, so a truncated or
// tampered file is caught rather than silently replaced. On first run the
// directory is created if needed and the identity is claimed with an
// exclusive create, so a daemon and a CLI invocation racing on the same
// state directory converge on one identity rather than each writing its own.
func LoadOrCreate(stateDir string) (string, error) {
	path := filepath.Join(stateDir, FileName)

	id, err := readValid(path)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return "", fmt.Errorf("hostid: create state dir %q: %w", stateDir, err)
	}

	fresh, err := Generate()
	if err != nil {
		return "", err
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process claimed it between our read and our create.
			// Its value is authoritative.
			return readValid(path)
		}
		return "", fmt.Errorf("hostid: create %q: %w", path, err)
	}
	if _, werr := f.WriteString(fresh + "\n"); werr != nil {
		f.Close()
		return "", fmt.Errorf("hostid: write %q: %w", path, werr)
	}
	if cerr := f.Close(); cerr != nil {
		return "", fmt.Errorf("hostid: close %q: %w", path, cerr)
	}
	return fresh, nil
}

// readValid reads path, trims a single trailing newline, and validates the
// result against the frozen format. A missing file returns an error wrapping
// os.ErrNotExist so LoadOrCreate can tell "first run" from "unreadable".
func readValid(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := string(data)
	if len(id) > 0 && id[len(id)-1] == '\n' {
		id = id[:len(id)-1]
	}
	if !Valid(id) {
		return "", fmt.Errorf("hostid: stored identity in %q is not a valid host_id (%q); refusing to overwrite it", path, id)
	}
	return id, nil
}
