// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 techgaud

package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/tagwright/core/runtime"
	"github.com/tagwright/core/runtime/runtimetest"
)

// requireRuntimeCapabilities is the boot-time skew guard added when verify grew
// its throwaway-container and network-isolation needs: a runtime adapter that
// silently dropped one of the optional Provisioner or NetworkInspector methods
// must fail loudly at startup, never start a daemon whose scheduled verify path
// would then panic or quietly do nothing. These tests drive that guard with
// fakes shaped to be missing exactly one capability, asserting the omission
// surfaces as a named error rather than a silent accept.

// bareRuntime implements runtime.Runtime and nothing more: it deliberately
// omits the optional Provisioner and NetworkInspector capabilities, so it is the
// stand-in for a ballast-side wrapper that dropped an optional method.
type bareRuntime struct{}

func (bareRuntime) List(context.Context) ([]runtime.Container, error) { return nil, nil }
func (bareRuntime) Inspect(context.Context, string) (runtime.Container, error) {
	return runtime.Container{}, nil
}
func (bareRuntime) Watch(context.Context) (<-chan runtime.Event, <-chan error) { return nil, nil }
func (bareRuntime) Exec(context.Context, string, runtime.ExecSpec) (*runtime.ExecHandle, error) {
	return nil, nil
}
func (bareRuntime) Stop(context.Context, string, int) error    { return nil }
func (bareRuntime) Start(context.Context, string) error        { return nil }
func (bareRuntime) Kill(context.Context, string, string) error { return nil }
func (bareRuntime) Restart(context.Context, string) error      { return nil }
func (bareRuntime) Close() error                               { return nil }

// provisionerOnlyRuntime satisfies Runtime and Provisioner but NOT
// NetworkInspector, isolating the second capability check from the first.
type provisionerOnlyRuntime struct{ bareRuntime }

func (provisionerOnlyRuntime) PullImage(context.Context, string) error { return nil }
func (provisionerOnlyRuntime) CreateNetwork(context.Context, runtime.NetworkSpec) (string, error) {
	return "", nil
}
func (provisionerOnlyRuntime) RemoveNetwork(context.Context, string) error { return nil }
func (provisionerOnlyRuntime) CreateVolume(context.Context, runtime.VolumeSpec) (string, error) {
	return "", nil
}
func (provisionerOnlyRuntime) RemoveVolume(context.Context, string) error { return nil }
func (provisionerOnlyRuntime) CreateContainer(context.Context, runtime.ContainerSpec) (string, error) {
	return "", nil
}
func (provisionerOnlyRuntime) RemoveContainer(context.Context, string, bool) error { return nil }

// TestRequireRuntimeCapabilities_AcceptsFullRuntime proves the guard passes a
// runtime that satisfies both optional capabilities: the canonical fake (like
// the real Docker and Podman adapters) implements Provisioner and
// NetworkInspector, so the guard must not reject it.
func TestRequireRuntimeCapabilities_AcceptsFullRuntime(t *testing.T) {
	if err := requireRuntimeCapabilities(runtimetest.New()); err != nil {
		t.Fatalf("a runtime satisfying Provisioner and NetworkInspector was rejected: %v", err)
	}
}

// TestRequireRuntimeCapabilities_RejectsMissingProvisioner proves a runtime that
// lacks the Provisioner capability is rejected at boot with an error that names
// the missing capability, rather than being accepted and blowing up later in the
// verify path.
func TestRequireRuntimeCapabilities_RejectsMissingProvisioner(t *testing.T) {
	err := requireRuntimeCapabilities(bareRuntime{})
	if err == nil {
		t.Fatal("a runtime lacking Provisioner was accepted: the boot-time capability guard did not fire")
	}
	if !strings.Contains(err.Error(), "Provisioner") {
		t.Fatalf("capability error does not name the missing Provisioner capability: %v", err)
	}
}

// TestRequireRuntimeCapabilities_RejectsMissingNetworkInspector proves a runtime
// that has Provisioner but lacks NetworkInspector is still rejected, and by name,
// so the second capability check is not shadowed by the first.
func TestRequireRuntimeCapabilities_RejectsMissingNetworkInspector(t *testing.T) {
	err := requireRuntimeCapabilities(provisionerOnlyRuntime{})
	if err == nil {
		t.Fatal("a runtime lacking NetworkInspector was accepted: the boot-time capability guard did not fire")
	}
	if !strings.Contains(err.Error(), "NetworkInspector") {
		t.Fatalf("capability error does not name the missing NetworkInspector capability: %v", err)
	}
}
