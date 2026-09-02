// SPDX-License-Identifier: GPL-3.0-or-later

package verify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tagwright/ballast/internal/engine"
	"github.com/tagwright/core/runtime"
)

// fakeEngine is an in-memory backup engine: Snapshots returns a fixed list and
// Restore materializes a set of files (keyed by absolute path) under the
// restore target, so both files-mode and stream-restore mode can be exercised
// without a real repository.
type fakeEngine struct {
	snaps      []engine.Snapshot
	files      map[string][]byte // absolute source path -> content, laid down under Target
	restoreErr error
}

func (f *fakeEngine) Name() string { return "restic" }

func (f *fakeEngine) Version(context.Context) string { return "0.19.1" }

func (f *fakeEngine) Snapshots(context.Context, engine.Repo) ([]engine.Snapshot, error) {
	return f.snaps, nil
}

func (f *fakeEngine) Restore(_ context.Context, req engine.RestoreRequest) error {
	if f.restoreErr != nil {
		return f.restoreErr
	}
	for abs, content := range f.files {
		dst := filepath.Join(req.Target, abs)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// execResult is a canned exec outcome the fake runtime returns for a command
// matched by substring.
type execResult struct {
	match  string
	stdout string
	exit   int
}

// fakeRuntime satisfies runtime.Runtime, runtime.Provisioner, and
// runtime.NetworkInspector. Provisioning calls succeed and are recorded so a
// test can assert every created object was torn down. Exec dispatches on the
// command's content via the execs table.
type fakeRuntime struct {
	mu sync.Mutex

	container runtime.Container
	pullErr   error
	execs     []execResult

	createdNets, createdVols, createdConts []string
	removedNets, removedVols, removedConts []string
}

func (f *fakeRuntime) List(context.Context) ([]runtime.Container, error) {
	return []runtime.Container{f.container}, nil
}
func (f *fakeRuntime) Inspect(_ context.Context, id string) (runtime.Container, error) {
	// Used only by the idempotent teardown when a remove errors; the fake never
	// errors on remove, so a "gone" answer here is safe.
	return runtime.Container{}, fmt.Errorf("no such container %s", id)
}
func (f *fakeRuntime) Watch(context.Context) (<-chan runtime.Event, <-chan error) { return nil, nil }
func (f *fakeRuntime) Stop(context.Context, string, int) error                    { return nil }
func (f *fakeRuntime) Start(context.Context, string) error                        { return nil }
func (f *fakeRuntime) Kill(context.Context, string, string) error                 { return nil }
func (f *fakeRuntime) Restart(context.Context, string) error                      { return nil }
func (f *fakeRuntime) Close() error                                               { return nil }

func (f *fakeRuntime) Exec(_ context.Context, _ string, spec runtime.ExecSpec) (*runtime.ExecHandle, error) {
	// Drain any stdin (a dump import) to mimic the real adapter closing the
	// write side after copying.
	if spec.Stdin != nil {
		_, _ = io.Copy(io.Discard, spec.Stdin)
	}
	cmd := ""
	if len(spec.Cmd) > 0 {
		cmd = spec.Cmd[len(spec.Cmd)-1]
	}
	stdout, exit := "", 0
	for _, e := range f.execs {
		if e.match != "" && bytesContains(cmd, e.match) {
			stdout, exit = e.stdout, e.exit
			break
		}
	}
	return handleFor(stdout, exit), nil
}

func handleFor(stdout string, exit int) *runtime.ExecHandle {
	return &runtime.ExecHandle{
		Stdout: bytes.NewReader([]byte(stdout)),
		Wait: func() (int, error) {
			if exit != 0 {
				return exit, fmt.Errorf("exited %d", exit)
			}
			return 0, nil
		},
	}
}

func bytesContains(s, sub string) bool { return bytes.Contains([]byte(s), []byte(sub)) }

// Provisioner.

func (f *fakeRuntime) PullImage(context.Context, string) error { return f.pullErr }

func (f *fakeRuntime) CreateNetwork(_ context.Context, spec runtime.NetworkSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdNets = append(f.createdNets, spec.Name)
	return spec.Name, nil
}
func (f *fakeRuntime) RemoveNetwork(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedNets = append(f.removedNets, id)
	return nil
}
func (f *fakeRuntime) CreateVolume(_ context.Context, spec runtime.VolumeSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createdVols = append(f.createdVols, spec.Name)
	return spec.Name, nil
}
func (f *fakeRuntime) RemoveVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedVols = append(f.removedVols, name)
	return nil
}
func (f *fakeRuntime) CreateContainer(_ context.Context, spec runtime.ContainerSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := spec.Name
	f.createdConts = append(f.createdConts, id)
	return id, nil
}
func (f *fakeRuntime) RemoveContainer(_ context.Context, id string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removedConts = append(f.removedConts, id)
	return nil
}

// NetworkInspector.

func (f *fakeRuntime) ListNetworks(context.Context) ([]runtime.Network, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var nets []runtime.Network
	for _, n := range f.createdNets {
		nets = append(nets, runtime.Network{Name: n, ID: n, Internal: true})
	}
	return nets, nil
}

// leaked reports whether any created throwaway object was not later removed,
// the leak check every scenario asserts.
func (f *fakeRuntime) leaked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var leaks []string
	count := func(kind string, created, removed []string) {
		rem := map[string]int{}
		for _, r := range removed {
			rem[r]++
		}
		for _, c := range created {
			if rem[c] == 0 {
				leaks = append(leaks, kind+":"+c)
				continue
			}
			rem[c]--
		}
	}
	count("network", f.createdNets, f.removedNets)
	count("volume", f.createdVols, f.removedVols)
	count("container", f.createdConts, f.removedConts)
	return leaks
}

// fixedClock returns a clock that advances by step on each call, so durations
// come out deterministic and non-zero.
func fixedClock(start time.Time, step time.Duration) func() time.Time {
	cur := start
	return func() time.Time {
		t := cur
		cur = cur.Add(step)
		return t
	}
}
