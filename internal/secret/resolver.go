// SPDX-License-Identifier: GPL-3.0-or-later

// Package secret resolves the named-secret references that labels and
// Ballast's own config carry, and derives per-service repository passwords
// from a single master secret. Nothing in this package ever holds a secret
// value longer than it has to, and no secret value is ever logged.
package secret

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultSecretsDir is used when a caller does not set BALLAST_SECRETS_DIR
// or otherwise configure a directory.
const DefaultSecretsDir = "/run/ballast/secrets"

// Resolver resolves a named secret to its value. It is the seam through
// which every named-secret reference in a label or in Ballast's config gets
// its value: labels and config name secrets, they never contain them.
//
// This signature intentionally matches beacon.SecretResolver so the same
// Resolver can be handed straight to the notification and telemetry module
// once Ballast wires it in.
type Resolver func(name string) (string, error)

// FileEnvResolver returns a Resolver that looks up name first as a file
// under secretsDir, then as an environment variable.
//
// Resolution order, matching the grammar's "Secret references" contract:
//  1. File filepath.Join(secretsDir, name).
//  2. Env var BALLAST_SECRET_<NAME>, where NAME is name uppercased with
//     "-" replaced by "_".
//  3. Neither found: an error naming the secret, so the caller can skip and
//     alert on the owning service rather than fail silently.
//
// Whichever source a value comes from, it is trimmed the same way: leading
// and trailing whitespace (spaces, tabs, CR, LF) is stripped before it is
// returned. This keeps a secret's resolved value identical regardless of
// which source supplied it, so a value copy-pasted into an env var with a
// stray trailing newline behaves the same as one read from a file. It
// matters most for the master key: LoadMaster hands the resolved bytes
// straight to HKDF as IKM, so an untrimmed source would silently derive
// different repository passwords than a trimmed one.
//
// secretsDir defaults to DefaultSecretsDir when empty.
func FileEnvResolver(secretsDir string) Resolver {
	if secretsDir == "" {
		secretsDir = DefaultSecretsDir
	}

	return func(name string) (string, error) {
		if name == "" {
			return "", fmt.Errorf("secret: empty secret name")
		}

		path := filepath.Join(secretsDir, name)
		if data, err := os.ReadFile(path); err == nil {
			return trimSecret(string(data)), nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("secret: read %s: %w", path, err)
		}

		envName := envVarName(name)
		if v, ok := os.LookupEnv(envName); ok {
			return trimSecret(v), nil
		}

		return "", fmt.Errorf("secret: %q not found in %s or %s", name, path, envName)
	}
}

// trimSecret strips leading and trailing whitespace (spaces, tabs, CR, LF)
// from a resolved secret value. It is applied uniformly to every source a
// Resolver can pull from, so the resolved value never depends on which
// source supplied it.
func trimSecret(v string) string {
	return strings.Trim(v, "\r\n \t")
}

// envVarName maps a secret name to the BALLAST_SECRET_<NAME> env var Ballast
// falls back to when no secrets-directory file exists for it.
func envVarName(name string) string {
	upper := strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
	return "BALLAST_SECRET_" + upper
}
