// SPDX-License-Identifier: GPL-3.0-or-later

package verify

import (
	"errors"
	"fmt"
	"os/exec"

	"github.com/tagwright/ballast/internal/record"
)

// decideProbe turns a probe's exit code and stdout into the record's verdict,
// the one place a verify becomes pass or fail: a non-zero exit is probe_failed,
// a zero exit whose stdout does not satisfy a declared expect is
// expect_mismatch, and otherwise it is a pass. It also records checked.rows when
// the stdout is a bare count.
func (r *run) decideProbe(exit int, stdout string) {
	if n, ok := parseRows(stdout); ok {
		r.v.Checked["rows"] = n
	}

	if exit != 0 {
		r.fail("probe_failed", fmt.Sprintf("probe exited %d", exit))
		return
	}
	if r.spec.Verify.Expect != "" && !expectMatches(r.spec.Verify.Expect, stdout) {
		r.fail("expect_mismatch", fmt.Sprintf("probe stdout %q did not match expect %q", trimForReason(stdout), r.spec.Verify.Expect))
		return
	}
	r.pass()
}

// probeOutput builds the record's bounded probe capture: the exit code, the
// excerpt, and a digest over the full stdout. stderr is null because the
// runtime exec handle does not surface it separately.
func probeOutput(exit int, cw *captureWriter) *record.CommandOutput {
	e := exit
	excerpt := cw.text()
	sha := cw.sha()
	return &record.CommandOutput{
		Exit:          &e,
		StdoutExcerpt: &excerpt,
		StdoutSHA256:  &sha,
		StderrExcerpt: nil,
	}
}

// clamp255 forces an exit code into the record's 0..255 range. A process killed
// by a signal reports -1; that and any out-of-range value become 1.
func clamp255(code int) int {
	if code < 0 || code > 255 {
		return 1
	}
	return code
}

// asExitError reports whether err (or something it wraps) is an *exec.ExitError,
// storing it in target.
func asExitError(err error, target **exec.ExitError) bool {
	return errors.As(err, target)
}

// trimForReason bounds a stdout snippet folded into a human reason string so a
// large probe output can't blow up the record's reason field.
func trimForReason(s string) string {
	const max = 200
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
