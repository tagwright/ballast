// SPDX-License-Identifier: GPL-3.0-or-later

package verify

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/record"
	"github.com/tagwright/core/runtime"
)

// runStreamRestore implements stream-restore mode: restore a streamed dump to
// scratch, boot a throwaway container of the image on an isolated network, pipe
// the dump into the restore client on its stdin, then run the probe inside and
// capture its output. The dump never touches a real service or a routable
// network.
func (r *run) runStreamRestore() {
	vspec := r.spec.Verify

	prov, ok := r.d.Runtime.(runtime.Provisioner)
	if !ok {
		r.inconclusive("runtime_unavailable", "runtime does not support provisioning throwaway containers")
		return
	}
	image := firstNonEmpty(vspec.Image, r.spec.Image)
	if image == "" {
		r.inconclusive("other", "no throwaway image: set verify.image or ensure the service image is discoverable")
		return
	}

	name := r.throwawayName()
	netName := name + "-net"
	imgCopy := image
	netCopy := netName
	r.v.Image = &imgCopy
	r.v.Environment = record.Environment{
		Kind:            "throwaway-container",
		Location:        name,
		Image:           &imgCopy,
		Network:         &netCopy,
		NetworkIsolated: true,
	}

	snap, ok := r.resolveSnapshot(r.v.SnapshotRequested)
	if !ok {
		return
	}

	// Pull first, so an unreachable registry is an image_unavailable
	// inconclusive before any restore work (matching the golden fixtures).
	if err := prov.PullImage(r.vctx, image); err != nil {
		code := r.ctxReasonCode(err, "other", "image_unavailable")
		r.inconclusive(code, fmt.Sprintf("pull %s: %v", image, err))
		return
	}

	if err := r.makeScratchDir(); err != nil {
		r.inconclusive("scratch_unavailable", fmt.Sprintf("create scratch directory: %v", err))
		return
	}

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

	dumpPath, dumpSize, derr := findDumpFile(r.scratchDir)
	if derr != nil {
		r.inconclusive("restore_failed", fmt.Sprintf("locate restored dump: %v", derr))
		return
	}
	dumpName := filepath.Base(dumpPath)
	r.v.Restored = record.Restored{Kind: "stream", Items: []string{dumpName}}
	r.v.Dataset = streamDataset(r.spec.Service, vspec.DataEngine, dumpName)
	r.v.Checked["files"] = 0
	r.v.Checked["bytes"] = dumpSize

	netID, err := prov.CreateNetwork(r.vctx, runtime.NetworkSpec{Name: netName, Labels: r.labels()})
	if err != nil {
		code := r.ctxReasonCode(err, "other", "scratch_unavailable")
		r.inconclusive(code, fmt.Sprintf("create isolated network: %v", err))
		return
	}
	r.addNetworkTeardown(prov, netID)
	r.confirmIsolated(netName)

	contID, err := prov.CreateContainer(r.vctx, runtime.ContainerSpec{
		Name:    name,
		Image:   image,
		Env:     envSlice(vspec.Env),
		Labels:  r.labels(),
		Network: netName,
		Start:   true,
	})
	if contID != "" {
		r.addContainerTeardown(prov, contID)
	}
	if err != nil {
		code := r.ctxReasonCode(err, "other", "other")
		r.inconclusive(code, fmt.Sprintf("create/start throwaway container: %v", err))
		return
	}

	if err := waitReady(r.vctx, r.d.Runtime, contID, vspec.Ready, vspec.User, remainingTimeout(r.vctx)); err != nil {
		code := r.ctxReasonCode(err, "other", "other")
		r.inconclusive(code, fmt.Sprintf("throwaway container did not become ready: %v", err))
		return
	}

	f, ferr := os.Open(dumpPath)
	if ferr != nil {
		r.inconclusive("restore_failed", fmt.Sprintf("open restored dump: %v", ferr))
		return
	}
	defer f.Close()

	impExit, _, impErr := execCapture(r.vctx, r.d.Runtime, contID, vspec.Restore, vspec.User, f)
	if impErr != nil {
		code := r.ctxReasonCode(impErr, "restore_timeout", "restore_failed")
		r.inconclusive(code, fmt.Sprintf("import dump into throwaway container: %v", impErr))
		return
	}
	if impExit != 0 {
		r.inconclusive("restore_failed", fmt.Sprintf("dump import exited %d", impExit))
		return
	}

	r.assertContainerProbe(contID)
}

// streamDataset renders the human dataset text for a stream restore, folding in
// the data-engine hint when the operator supplied one.
func streamDataset(service, dataEngine, dumpName string) string {
	if dataEngine != "" {
		return fmt.Sprintf("%s (%s stream: %s)", service, dataEngine, dumpName)
	}
	return fmt.Sprintf("%s (stream: %s)", service, dumpName)
}
