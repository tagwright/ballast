// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package verify

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/ballast/internal/record"
	"github.com/tagwright/core/runtime"
)

// runContainer implements container mode: restore the service's volume data
// into fresh scratch volumes (never the real ones), boot a throwaway copy of the
// image with those volumes attached on an isolated network, and run the probe
// inside it.
//
// A fresh named volume cannot be written to from the ballast host through the
// runtime interface, so the restored bytes are streamed into each volume via a
// short-lived populator container (the image's own shell running tar -x), which
// is torn down before the real container boots on the now-populated volumes.
func (r *run) runContainer() {
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
	vols := sortedVolumeMounts(r.container)
	if len(vols) == 0 {
		r.inconclusive("other", "container mode needs at least one named-volume mount to restore into")
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
	dests := make([]string, len(vols))
	for i, m := range vols {
		dests[i] = m.Destination
	}
	r.v.Restored = record.Restored{Kind: "volumes", Items: dests}
	r.v.Dataset = fmt.Sprintf("%s (volumes: %s)", r.spec.Service, strings.Join(dests, ", "))

	snap, ok := r.resolveSnapshot(r.v.SnapshotRequested)
	if !ok {
		return
	}

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
	r.v.Checked["files"] = 0

	netID, err := prov.CreateNetwork(r.vctx, runtime.NetworkSpec{Name: netName, Labels: r.labels()})
	if err != nil {
		code := r.ctxReasonCode(err, "other", "scratch_unavailable")
		r.inconclusive(code, fmt.Sprintf("create isolated network: %v", err))
		return
	}
	r.addNetworkTeardown(prov, netID)
	r.confirmIsolated(netName)

	// Create one fresh scratch volume per service volume, mounted at the same
	// destination the real service uses.
	mounts := make([]runtime.VolumeMount, 0, len(vols))
	for i, m := range vols {
		volName := fmt.Sprintf("%s-vol%d", name, i)
		created, verr := prov.CreateVolume(r.vctx, runtime.VolumeSpec{Name: volName, Labels: r.labels()})
		if verr != nil {
			code := r.ctxReasonCode(verr, "other", "scratch_unavailable")
			r.inconclusive(code, fmt.Sprintf("create scratch volume: %v", verr))
			return
		}
		r.addVolumeTeardown(prov, created)
		mounts = append(mounts, runtime.VolumeMount{Volume: created, Destination: m.Destination})
	}

	// Populator: the image's shell, kept alive, with the scratch volumes
	// mounted, so restored bytes can be streamed into each volume via tar.
	popID, err := prov.CreateContainer(r.vctx, runtime.ContainerSpec{
		Name:       name + "-pop",
		Image:      image,
		Entrypoint: []string{"/bin/sh", "-c"},
		Cmd:        []string{"sleep 3600"},
		Labels:     r.labels(),
		Network:    netName,
		Mounts:     mounts,
		Start:      true,
	})
	if popID != "" {
		r.addContainerTeardown(prov, popID)
	}
	if err != nil {
		code := r.ctxReasonCode(err, "other", "other")
		r.inconclusive(code, fmt.Sprintf("create/start populator container: %v", err))
		return
	}

	for _, m := range vols {
		srcDir := filepath.Join(r.scratchDir, m.Source)
		if err := r.tarInto(popID, srcDir, m.Destination, vspec.User); err != nil {
			code := r.ctxReasonCode(err, "restore_timeout", "restore_failed")
			r.inconclusive(code, fmt.Sprintf("populate scratch volume for %s: %v", m.Destination, err))
			return
		}
	}

	// The populator must be gone before the real container boots on the same
	// volumes; the idempotent teardown tolerates this early removal.
	if err := prov.RemoveContainer(r.vctx, popID, true); err != nil {
		r.log.Warn("verify: could not remove populator before boot", "service", r.spec.Service, "error", err)
	}

	contID, err := prov.CreateContainer(r.vctx, runtime.ContainerSpec{
		Name:    name,
		Image:   image,
		Env:     envSlice(vspec.Env),
		Labels:  r.labels(),
		Network: netName,
		Mounts:  mounts,
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

	r.assertContainerProbe(contID)
}

// tarInto streams a tar of srcDir's contents into the container and extracts it
// at dest via the image's own tar, populating a scratch volume with restored
// data. The container image must provide tar (the common database images do).
func (r *run) tarInto(containerID, srcDir, dest, user string) error {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(writeTar(pw, srcDir))
	}()

	exit, _, transportErr := execCapture(r.vctx, r.d.Runtime, containerID,
		fmt.Sprintf("tar -x -C %s", shellQuote(dest)), user, pr)
	if transportErr != nil {
		return transportErr
	}
	if exit != 0 {
		return fmt.Errorf("tar extract exited %d", exit)
	}
	return nil
}
