// SPDX-License-Identifier: GPL-3.0-or-later

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

	return []byte(v), nil
}
