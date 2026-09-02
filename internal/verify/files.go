// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/manifest"
	"github.com/tagwright/ballast/internal/record"
)

// runFiles implements files mode: restore the snapshot to a fresh scratch
// directory and diff the restored tree against the backup-time manifest, filling
// manifest_compare. No container is ever created. If a probe is declared it runs
// against the scratch directory and decides the outcome; otherwise the manifest
// diff decides it.
func (r *run) runFiles() {
	vspec := r.spec.Verify

	snap, ok := r.resolveSnapshot(r.v.SnapshotRequested)
	if !ok {
		return
	}

	if err := r.makeScratchDir(); err != nil {
		r.inconclusive("scratch_unavailable", fmt.Sprintf("create scratch directory: %v", err))
		return
	}
	r.v.Environment.Location = r.scratchDir
	r.v.Restored = record.Restored{Kind: "paths", Items: nonEmptyStrings(r.spec.Paths)}
	r.v.Dataset = fmt.Sprintf("%s (files: %d paths)", r.spec.Service, len(r.v.Restored.Items))

	restoreStart := r.d.Now()
	err := r.d.Engine.Restore(r.vctx, engine.RestoreRequest{
		Repo:       r.d.Repo,
		SnapshotID: snap.ID,
		Target:     r.scratchDir,
	})
	r.v.RestoreDurationMs = durMs(r.d.Now().Sub(restoreStart))
	if err != nil {
		code := r.ctxReasonCode(err, "restore_timeout", "restore_failed")
		r.inconclusive(code, fmt.Sprintf("restore snapshot %s: %v", snap.ID, err))
		return
	}

	mc, haveManifest := r.compareManifest(snap.ID)
	if haveManifest {
		r.v.ManifestCompare = mc
		r.v.Checked["files"] = mc.EntriesExpected
		var bytes uint64
		if entries, err := r.manifestEntries(snap.ID); err == nil {
			for _, e := range entries {
				bytes += e.Size
			}
		}
		r.v.Checked["bytes"] = bytes
	}

	if vspec.Probe != "" {
		r.assertHostProbe()
		return
	}

	// Manifest-only assertion.
	if !haveManifest {
		r.inconclusive("other", fmt.Sprintf("no backup-time manifest recorded for snapshot %s; nothing to assert against", snap.ID))
		return
	}
	if mc.Missing > 0 || mc.Mismatched > 0 {
		r.fail("manifest_mismatch", fmt.Sprintf("restored tree diverges from the backup-time manifest: %d missing, %d mismatched of %d files",
			mc.Missing, mc.Mismatched, mc.EntriesExpected))
		return
	}
	r.pass()
}

// assertHostProbe runs the declared probe against the scratch directory on the
// ballast host (its working directory is the scratch dir) and decides the
// outcome. This executes an operator-declared shell command on the host, so it
// is only ever the operator's own verify.probe, run against a copy of their own
// data.
func (r *run) assertHostProbe() {
	vspec := r.spec.Verify
	start := r.d.Now()

	cmd := exec.CommandContext(r.vctx, "sh", "-c", vspec.Probe)
	cmd.Dir = r.scratchDir
	cw := newCapture()
	cmd.Stdout = cw
	cmd.Stderr = io.Discard

	runErr := cmd.Run()
	r.v.ProbeDurationMs = durMs(r.d.Now().Sub(start))

	exit := 0
	var transportErr error
	if runErr != nil {
		var exitErr *exec.ExitError
		if ok := asExitError(runErr, &exitErr); ok {
			exit = clamp255(exitErr.ExitCode())
		} else {
			transportErr = runErr
		}
	}

	r.v.ProbeOutput = probeOutput(exit, cw)

	if transportErr != nil {
		code := r.ctxReasonCode(transportErr, "probe_timeout", "other")
		r.inconclusive(code, fmt.Sprintf("probe could not run: %v", transportErr))
		return
	}
	r.decideProbe(exit, cw.text())
}

// compareManifest loads the backup-time manifest for snapshotID and diffs the
// restored scratch tree against it. haveManifest is false (and mc nil) when no
// run record links a manifest to this snapshot.
func (r *run) compareManifest(snapshotID string) (mc *record.ManifestCompare, haveManifest bool) {
	entries, err := r.manifestEntries(snapshotID)
	if err != nil || entries == nil {
		return nil, false
	}

	expected := make(map[string]struct{}, len(entries))
	var matched, mismatched, missing uint64
	for _, e := range entries {
		restored := filepath.Join(r.scratchDir, e.Path)
		expected[restored] = struct{}{}
		info, statErr := os.Stat(restored)
		if statErr != nil {
			missing++
			continue
		}
		if uint64(info.Size()) != e.Size {
			mismatched++
			continue
		}
		sum, herr := hashFileHex(restored)
		if herr != nil || "sha256:"+sum != e.SHA256 {
			mismatched++
			continue
		}
		matched++
	}

	var extra uint64
	_ = filepath.WalkDir(r.scratchDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if _, ok := expected[path]; !ok {
			extra++
		}
		return nil
	})

	return &record.ManifestCompare{
		EntriesExpected: uint64(len(entries)),
		EntriesMatched:  matched,
		Mismatched:      mismatched,
		Missing:         missing,
		Extra:           extra,
	}, true
}

// manifestEntries finds the backup-time manifest linked to snapshotID by
// scanning the service's run records for one whose snapshot_id matches and that
// recorded a manifest, then loads that manifest.
func (r *run) manifestEntries(snapshotID string) ([]manifest.Entry, error) {
	if r.d.StateDir == "" {
		return nil, nil
	}
	dir := filepath.Join(r.d.StateDir, "runs", r.spec.Service)
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	for _, f := range files {
		if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, f.Name()))
		if rerr != nil {
			continue
		}
		var rr record.Run
		if json.Unmarshal(data, &rr) != nil {
			continue
		}
		if rr.Manifest == nil || rr.SnapshotID == nil || *rr.SnapshotID != snapshotID {
			continue
		}
		return manifest.Load(rr.Manifest.Location)
	}
	return nil, nil
}

// hashFileHex returns the lowercase hex SHA-256 of a file's contents.
func hashFileHex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// nonEmptyStrings returns s with empty entries dropped, and an empty (non-nil)
// slice when nothing remains, so the record's items array is never null and
// never carries an empty string (which the schema forbids).
func nonEmptyStrings(s []string) []string {
	out := make([]string, 0, len(s))
	for _, v := range s {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
