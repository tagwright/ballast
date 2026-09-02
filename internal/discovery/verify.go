// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"fmt"
	"strings"
	"time"
)

// VerifyMode is the single closed enum on the verify seam: the three
// mechanisms ballast can use to prove a snapshot restores. It is deliberately
// about mechanism, never about a data engine (postgres vs mysql is a different
// image and probe, not a different mode).
type VerifyMode string

const (
	// VerifyModeFiles restores to a scratch directory and diffs the restored
	// tree against the backup-time manifest. No container.
	VerifyModeFiles VerifyMode = "files"
	// VerifyModeContainer restores the service's volume data into fresh scratch
	// volumes, boots a throwaway copy of the image with them attached on an
	// isolated network, and runs the probe inside it.
	VerifyModeContainer VerifyMode = "container"
	// VerifyModeStreamRestore restores a streamed dump to scratch, boots a
	// throwaway container of the image on an isolated network, pipes the dump
	// into the restore client on its stdin, then runs the probe inside.
	VerifyModeStreamRestore VerifyMode = "stream-restore"
)

// defaultVerifyTimeout bounds the whole verify (image pull, container start,
// restore, and probe) when verify.timeout is not set.
const defaultVerifyTimeout = 10 * time.Minute

// VerifySpec is a service's fully resolved verify configuration, parsed from
// its verify.* labels. Configured reports whether any verify.* label was
// present at all (the opt-in trigger for the backup-time manifest); the other
// fields are only meaningful when Configured is true.
//
// Everything that names a data engine here is a free string supplied by the
// operator (Image, Probe, Restore, DataEngine, Env): ballast interprets none of
// them and special-cases no engine. Mode is the only closed vocabulary.
type VerifySpec struct {
	Configured bool

	Mode    VerifyMode
	Probe   string
	Expect  string
	Timeout time.Duration

	// Image overrides the throwaway container image for container and
	// stream-restore modes. Empty means fall back to the service's own image
	// (BackupSpec.Image).
	Image string

	// Schedule is an optional local cron/alias for a single-host operator to
	// run this verify on. Empty means no local schedule (Billet drives verify
	// fleet-wide instead).
	Schedule string

	// DataEngine is an informational free-string hint recorded as the record's
	// data_engine (postgres, mariadb, mongo, files, ...). Never load bearing,
	// never validated against a list.
	DataEngine string

	// Restore is the stream-restore dump-ingest command, run inside the
	// throwaway container with the restored dump piped to its stdin (for
	// example "psql -U nextcloud -d nextcloud"). Required for stream-restore
	// mode, ignored otherwise.
	Restore string

	// Ready is an optional readiness command run inside the throwaway container
	// and polled until it exits zero (or the timeout elapses) before the dump
	// is piped in and the probe runs. Empty skips the readiness wait.
	Ready string

	// Env are environment entries (verify.env.<KEY>) for the throwaway
	// container, most commonly the credentials a database image needs to
	// initialize (POSTGRES_PASSWORD, ...). Never secrets resolved from a store:
	// a verify boots a throwaway copy, and these are its bootstrap values.
	Env map[string]string

	// User is the optional user the probe, restore, and ready commands run as
	// inside the throwaway container. Empty uses the image's default user.
	User string
}

// hasVerifyConfig reports whether norm carries any verify configuration: the
// bare "verify" suffix or any "verify.<field>" suffix.
func hasVerifyConfig(norm map[string]string) bool {
	for k := range norm {
		if k == "verify" || strings.HasPrefix(k, "verify.") {
			return true
		}
	}
	return false
}

// parseVerify builds a VerifySpec from the verify.* labels in norm. It applies
// the defaults (files mode, 10m timeout) and rejects a malformed mode, a bad
// timeout duration, or a stream-restore with no dump-ingest command. When no
// verify.* label is present it returns a zero-value spec with Configured false
// and the defaults filled, so a caller can read Mode/Timeout unconditionally.
func parseVerify(norm map[string]string) (VerifySpec, error) {
	v := VerifySpec{
		Configured: hasVerifyConfig(norm),
		Mode:       VerifyModeFiles,
		Timeout:    defaultVerifyTimeout,
	}
	if !v.Configured {
		return v, nil
	}

	if m := norm["verify.mode"]; m != "" {
		switch VerifyMode(m) {
		case VerifyModeFiles, VerifyModeContainer, VerifyModeStreamRestore:
			v.Mode = VerifyMode(m)
		default:
			return VerifySpec{}, fmt.Errorf("discovery: label %q: unknown verify mode %q, want files, container, or stream-restore", "verify.mode", m)
		}
	}

	timeout, err := parseDuration(norm, "verify.timeout", defaultVerifyTimeout)
	if err != nil {
		return VerifySpec{}, err
	}
	v.Timeout = timeout

	v.Probe = norm["verify.probe"]
	v.Expect = norm["verify.expect"]
	v.Image = norm["verify.image"]
	v.Schedule = norm["verify.schedule"]
	v.DataEngine = norm["verify.data-engine"]
	v.Restore = norm["verify.restore"]
	v.Ready = norm["verify.ready"]
	v.User = norm["verify.user"]
	v.Env = collectEnv(norm)

	if v.Mode == VerifyModeStreamRestore && v.Restore == "" {
		return VerifySpec{}, fmt.Errorf("discovery: verify.mode=stream-restore requires a verify.restore command to pipe the dump into")
	}

	return v, nil
}

// collectEnv gathers the verify.env.<KEY> labels into a KEY->value map,
// returning nil when none are present so an unset Env stays a nil map.
func collectEnv(norm map[string]string) map[string]string {
	var env map[string]string
	const prefix = "verify.env."
	for k, val := range norm {
		key, ok := strings.CutPrefix(k, prefix)
		if !ok || key == "" {
			continue
		}
		if env == nil {
			env = make(map[string]string)
		}
		env[key] = val
	}
	return env
}
