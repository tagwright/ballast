// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package verify

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tagwright/core/runtime"
)

// throwawayName builds a deterministic, docker-safe name for this verify's
// throwaway objects: <prefix>-<sanitized service>-<short verify id>. The suffix
// keeps concurrent verifies of the same service from colliding.
func (r *run) throwawayName() string {
	return fmt.Sprintf("%s-%s-%s", r.d.NamePrefix, sanitizeName(r.spec.Service), shortID(r.v.VerifyID))
}

// labels returns the label set stamped on every throwaway object so the orphan
// sweep can find leftovers.
func (r *run) labels() map[string]string {
	return map[string]string{labelKey: r.v.VerifyID}
}

// envSlice renders a verify.env.<KEY> map as a sorted KEY=VALUE slice for the
// throwaway container's environment.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

// addContainerTeardown registers an idempotent container removal: a container
// already gone (for example a populator removed early) is treated as removed
// rather than a teardown error.
func (r *run) addContainerTeardown(prov runtime.Provisioner, id string) {
	r.td.add(func(ctx context.Context) error {
		err := prov.RemoveContainer(ctx, id, true)
		if err == nil {
			return nil
		}
		if _, ierr := r.d.Runtime.Inspect(ctx, id); ierr != nil {
			// The container is no longer inspectable, so it is already gone.
			return nil
		}
		return fmt.Errorf("remove container %s: %w", id, err)
	})
}

// addNetworkTeardown registers removal of a throwaway network.
func (r *run) addNetworkTeardown(prov runtime.Provisioner, id string) {
	r.td.add(func(ctx context.Context) error {
		if err := prov.RemoveNetwork(ctx, id); err != nil {
			return fmt.Errorf("remove network %s: %w", id, err)
		}
		return nil
	})
}

// addVolumeTeardown registers removal of a throwaway named volume.
func (r *run) addVolumeTeardown(prov runtime.Provisioner, name string) {
	r.td.add(func(ctx context.Context) error {
		if err := prov.RemoveVolume(ctx, name); err != nil {
			return fmt.Errorf("remove volume %s: %w", name, err)
		}
		return nil
	})
}

// confirmIsolated verifies the created network reports Internal via the
// NetworkInspector capability, downgrading the record's network_isolated to
// false only if it can positively see a non-internal network. CreateNetwork
// always creates an internal network, so this is a defence-in-depth check that
// the segregation fact recorded is real, not an assumption.
func (r *run) confirmIsolated(netName string) {
	insp, ok := r.d.Runtime.(runtime.NetworkInspector)
	if !ok {
		return
	}
	nets, err := insp.ListNetworks(r.vctx)
	if err != nil {
		return
	}
	for _, n := range nets {
		if n.Name == netName {
			if !n.Internal {
				r.v.Environment.NetworkIsolated = false
				r.log.Error("verify: throwaway network is not internal; refusing to claim isolation",
					"service", r.spec.Service, "network", netName)
			}
			return
		}
	}
}

// assertContainerProbe runs the declared probe inside the throwaway container
// and decides the outcome. Shared by container and stream-restore modes.
func (r *run) assertContainerProbe(containerID string) {
	start := r.d.Now()
	exit, cw, transportErr := execCapture(r.vctx, r.d.Runtime, containerID, r.spec.Verify.Probe, r.spec.Verify.User, nil)
	r.v.ProbeDurationMs = durMs(r.d.Now().Sub(start))

	if transportErr != nil {
		code := r.ctxReasonCode(transportErr, "probe_timeout", "other")
		r.inconclusive(code, fmt.Sprintf("probe could not run: %v", transportErr))
		return
	}
	r.v.ProbeOutput = probeOutput(exit, cw)
	r.decideProbe(exit, cw.text())
}

// findDumpFile locates the single dump file a restored stream snapshot produced
// under scratchDir. A stream (stdin) snapshot restores to exactly one file; if
// several regular files are present (an unexpected shape) the largest is taken.
func findDumpFile(scratchDir string) (path string, size uint64, err error) {
	var best string
	var bestSize int64 = -1
	walkErr := filepath.WalkDir(scratchDir, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		if info.Size() > bestSize {
			best, bestSize = p, info.Size()
		}
		return nil
	})
	if walkErr != nil {
		return "", 0, walkErr
	}
	if best == "" {
		return "", 0, fmt.Errorf("no dump file found in restored snapshot")
	}
	return best, uint64(bestSize), nil
}

// writeTar streams a tar archive of srcDir's contents (entries relative to
// srcDir) into w, preserving file modes and ownership so a restored data
// directory extracts into a volume with the uids the source carried. It is the
// producer half of the tar-into-volume populate used by container mode.
func writeTar(w io.Writer, srcDir string) error {
	tw := tar.NewWriter(w)
	defer tw.Close()

	return filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(srcDir, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}

		var link string
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, herr := tar.FileInfoHeader(info, link)
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, oerr := os.Open(path)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		_, cerr := io.Copy(tw, f)
		return cerr
	})
}

// shellQuote single-quotes s for safe interpolation into a "sh -c" command.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
