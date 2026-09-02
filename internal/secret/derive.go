// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package secret

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// MasterSecretName is the name of the secret DeriveRepoPassword's master key
// is resolved from. It holds the single master value every per-service repo
// password is derived from (Fork 4 of the label grammar).
const MasterSecretName = "repo-master-key"

// ---------------------------------------------------------------------------
// FROZEN v1 CONTRACT
//
// The constants below (hash, salt, info template, output length, encoding)
// are a frozen v1 public contract. Changing any parameter below orphans
// every existing repository: DeriveRepoPassword must reproduce, byte for
// byte, the same password for the same (master, service) pair forever, or
// every repository built on top of it becomes unreadable without the exact
// old code path. If a change is ever unavoidable, it ships as a new,
// separately-named derivation (v2) that existing repos are migrated to
// deliberately, never as an edit in place here.
// ---------------------------------------------------------------------------

// repoKeyHKDFSalt is the fixed HKDF salt for repo password derivation. It is
// a constant, not a secret, and not per-installation: it exists only to
// domain-separate this derivation from any other HKDF use that might ever
// share the same master key.
const repoKeyHKDFSalt = "tagwright.ballast.repo-key.v1"

// repoKeyInfoPrefix prefixes the per-service HKDF info parameter, binding
// the derived key to exactly one service name.
const repoKeyInfoPrefix = "service:"

// repoKeyLength is the length in bytes of the derived key, before base64
// encoding.
const repoKeyLength = 32

// minMasterKeyBytes is the smallest master-key length LoadMaster accepts, in
// bytes of its resolved text form (see the LoadMaster doc comment). It is an
// edge guard around the frozen construction above, not part of the frozen
// contract itself: it exists to catch an obviously-too-short master (a typo,
// a placeholder value, a short password) before it is used as HKDF IKM,
// where HKDF would silently accept it and derive passwords from weak
// entropy. It sits at 32 to reject a hand-typed or placeholder master while
// comfortably admitting any master generated the recommended way: the ~44
// characters `openssl rand -base64 32` produces, a 64-character hex form, or
// 32 raw bytes all clear it.
const minMasterKeyBytes = 32

// DeriveRepoPassword derives the restic repository password for service from
// master, using HKDF-SHA256.
//
// Construction (frozen, see the v1 CONTRACT block above):
//   - Hash: SHA-256
//   - IKM (secret):  master
//   - salt:          []byte("tagwright.ballast.repo-key.v1")
//   - info:          []byte("service:" + service)
//   - output length: 32 bytes
//   - encoding:      base64.RawURLEncoding of the 32 output bytes
//
// The result is deterministic: the same master and service name always
// produce the same password, which is what makes recovery via `ballast key
// <service>` possible without any stored per-service state.
func DeriveRepoPassword(master []byte, service string) (string, error) {
	if len(master) == 0 {
		return "", fmt.Errorf("secret: derive repo password: empty master key")
	}
	if service == "" {
		return "", fmt.Errorf("secret: derive repo password: empty service name")
	}

	info := repoKeyInfoPrefix + service
	key, err := hkdf.Key(sha256.New, master, []byte(repoKeyHKDFSalt), info, repoKeyLength)
	if err != nil {
		return "", fmt.Errorf("secret: derive repo password for %q: %w", service, err)
	}

	return base64.RawURLEncoding.EncodeToString(key), nil
}

// LoadMaster resolves and returns the master key repo passwords are derived
// from, using resolve to look up MasterSecretName.
//
// The master must be a whitespace-free single-line text secret, such as the
// output of `openssl rand -base64 32`. Its text bytes are used verbatim as
// the HKDF IKM in DeriveRepoPassword: they are not base64-decoded, so the
// master is treated as an opaque string of bytes, not as encoded key
// material.
func LoadMaster(resolve Resolver) ([]byte, error) {
	if resolve == nil {
		return nil, fmt.Errorf("secret: load master: nil resolver")
	}

	v, err := resolve(MasterSecretName)
	if err != nil {
		return nil, fmt.Errorf("secret: load master: %w", err)
	}
	if v == "" {
		return nil, fmt.Errorf("secret: load master: %q resolved to an empty value", MasterSecretName)
	}
	if len(v) < minMasterKeyBytes {
		return nil, fmt.Errorf("secret: load master: %s too short (got %d bytes, need at least %d; generate with: openssl rand -base64 32)", MasterSecretName, len(v), minMasterKeyBytes)
	}

	return []byte(v), nil
}
