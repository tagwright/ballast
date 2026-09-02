// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tagwright/core/runtime"
)

// labelKey is stamped on every throwaway object (container, volume, network)
// a verify creates, with the verify_id as its value, so an orphan sweep can
// find and remove leftovers a crashed prior run left behind.
const labelKey = "ballast-verify"

// maxExcerpt bounds the probe stdout excerpt stored in the record, matching the
// contract's command_output.stdout_excerpt maxLength.
const maxExcerpt = 4096

// captureWriter hashes and counts an unbounded stdout stream in O(1) memory
// while retaining only the first maxExcerpt bytes for the record's excerpt. It
// is the sink execCapture copies a command's stdout into.
type captureWriter struct {
	h       hash.Hash
	n       uint64
	excerpt []byte
}

func newCapture() *captureWriter { return &captureWriter{h: sha256.New()} }

func (w *captureWriter) Write(p []byte) (int, error) {
	w.h.Write(p)
	w.n += uint64(len(p))
	if len(w.excerpt) < maxExcerpt {
		room := maxExcerpt - len(w.excerpt)
		if room > len(p) {
			room = len(p)
		}
		w.excerpt = append(w.excerpt, p[:room]...)
	}
	return len(p), nil
}

// sha returns the "sha256:" prefixed hex digest of everything written.
func (w *captureWriter) sha() string {
	return "sha256:" + hex.EncodeToString(w.h.Sum(nil))
}

// text returns the retained excerpt as a string.
func (w *captureWriter) text() string { return string(w.excerpt) }

// execCapture runs shellCmd through "sh -c" inside container id, streaming the
// command's stdout into a captureWriter, then waits for its exit.
//
// The contract of runtime.ExecHandle is that Stdout must be read to completion
// before Wait, or Wait deadlocks (the engine muxes stdout through an unbuffered
// pipe); execCapture always drains Stdout first. A returned transportErr means
// the exec could not be run, attached, or inspected (an inconclusive
// condition); a command that simply exited non-zero returns that exit code with
// a nil transportErr, because a non-zero probe is a verdict, not a harness
// failure.
func execCapture(ctx context.Context, rt runtime.Runtime, id, shellCmd, user string, stdin io.Reader) (exit int, cap *captureWriter, transportErr error) {
	hd, err := rt.Exec(ctx, id, runtime.ExecSpec{
		Cmd:   []string{"sh", "-c", shellCmd},
		User:  user,
		Stdin: stdin,
	})
	if err != nil {
		return 0, nil, err
	}
	cw := newCapture()
	_, rerr := io.Copy(cw, hd.Stdout)
	code, werr := hd.Wait()
	if rerr != nil {
		return code, cw, fmt.Errorf("read exec stdout: %w", rerr)
	}
	// Wait returns a non-nil error together with a non-zero code for a normal
	// non-zero exit (the message carries the command's stderr); that is a
	// verdict, not a transport failure. Only a non-nil error paired with a zero
	// code is a genuine transport problem (exec inspect failed).
	if werr != nil && code == 0 {
		return 0, cw, werr
	}
	return code, cw, nil
}

// waitReady polls readyCmd inside container id until it exits zero, the context
// is done, or maxWait elapses. A blank readyCmd means "no readiness gate": it
// returns nil immediately. It returns the context error on cancellation or
// deadline so the caller can classify a timeout.
func waitReady(ctx context.Context, rt runtime.Runtime, id, readyCmd, user string, maxWait time.Duration) error {
	if readyCmd == "" {
		return nil
	}
	deadline := time.Now().Add(maxWait)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		exit, _, transportErr := execCapture(ctx, rt, id, readyCmd, user, nil)
		if transportErr == nil && exit == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("readiness command did not succeed within %s", maxWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPollInterval):
		}
	}
}

// readyPollInterval is how often waitReady retries the readiness command.
const readyPollInterval = 500 * time.Millisecond

// remainingTimeout returns how long is left on ctx's deadline, or a small
// default when ctx has none, so a readiness wait never outlives the verify's
// own timeout.
func remainingTimeout(ctx context.Context) time.Duration {
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d > 0 {
			return d
		}
		return 0
	}
	return 30 * time.Second
}

// expectMatches reports whether the probe's (trimmed) stdout satisfies expect.
// expect is treated as a regular expression when it compiles as one (the
// documented and common case, e.g. "^[1-9][0-9]*$"), and as a plain substring
// otherwise, so a literal that happens not to be valid regex still works.
func expectMatches(expect, stdout string) bool {
	s := strings.TrimSpace(stdout)
	if re, err := regexp.Compile(expect); err == nil {
		return re.MatchString(s)
	}
	return strings.Contains(s, expect)
}

// parseRows returns the probe stdout as a row count when it is a bare
// non-negative integer (the count-query idiom), and ok=false otherwise, so the
// record's checked.rows is populated only when it is genuinely a count.
func parseRows(stdout string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(stdout), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// sanitizeName turns a service name (which may contain / . _ -) into a
// docker-safe name fragment: lowercase, with any character outside [a-z0-9] to
// a hyphen, bounded in length.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "svc"
	}
	if len(out) > 40 {
		out = strings.Trim(out[:40], "-")
	}
	return out
}

// shortID returns the lowercased tail of a ULID, used as the unique suffix on
// throwaway object names (matching the golden fixtures' naming).
func shortID(verifyID string) string {
	s := strings.ToLower(verifyID)
	if len(s) > 8 {
		return s[len(s)-8:]
	}
	return s
}

// strptr returns a pointer to s, or nil when s is empty, for the record's
// nullable string fields.
func strptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
