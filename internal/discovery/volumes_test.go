// SPDX-License-Identifier: GPL-3.0-or-later

package discovery

import (
	"os"
	"testing"

	"github.com/tagwright/ballast/internal/config"
	"github.com/tagwright/core/runtime"
)

// TestDiscoverDefaultHostRootsResolvesNamedVolume proves the "add one label
// and it starts getting backed up" promise: with an otherwise-empty config
// (no config file, no host_roots set), a named volume mounted the standard
// Docker way still resolves to a backupable path, because config.Load seeds
// HostRoots with the standard Docker volumes root by default.
func TestDiscoverDefaultHostRootsResolvesNamedVolume(t *testing.T) {
	cfg, err := config.Load("")
	if err != nil {
		t.Fatalf("config.Load(\"\"): %v", err)
	}

	c := runtime.Container{
		ID:     "c1",
		Name:   "silverbullet",
		Labels: map[string]string{"ballast.enable": "true"},
		Mounts: []runtime.Mount{
			{
				Type:        runtime.MountVolume,
				Name:        "sb-data",
				Source:      "/var/lib/docker/volumes/sb-data/_data",
				Destination: "/space",
			},
		},
	}

	spec, warnings, err := Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("Discover: unexpected warnings: %v", warnings)
	}
	if spec == nil {
		t.Fatal("Discover: spec is nil, want an enabled service's spec")
	}

	want := "/var/lib/docker/volumes/sb-data/_data"
	if len(spec.Paths) != 1 || spec.Paths[0] != want {
		t.Fatalf("spec.Paths = %v, want [%q]", spec.Paths, want)
	}
}

// TestDiscoverUserHostRootsMergeWithDefault proves a user's host_roots entry
// (set via a config file, exercised here through config.Load reading a temp
// YAML file) merges with, rather than replaces, the standard Docker volumes
// root config.Load seeds automatically: a bind-mount root the user adds
// resolves alongside a named volume with no host_roots entry of its own.
func TestDiscoverUserHostRootsMergeWithDefault(t *testing.T) {
	path := writeTempConfig(t, "host_roots:\n  /srv/binds: /srv/binds\n")

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load(%q): %v", path, err)
	}

	c := runtime.Container{
		ID:     "c2",
		Name:   "app",
		Labels: map[string]string{"ballast.enable": "true"},
		Mounts: []runtime.Mount{
			{
				Type:        runtime.MountVolume,
				Name:        "app-data",
				Source:      "/var/lib/docker/volumes/app-data/_data",
				Destination: "/data",
			},
			{
				Type:        runtime.MountBind,
				Source:      "/srv/binds/app-config",
				Destination: "/config",
			},
		},
	}

	spec, warnings, err := Discover(c, cfg)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("Discover: unexpected warnings: %v", warnings)
	}

	got := map[string]bool{}
	for _, p := range spec.Paths {
		got[p] = true
	}
	for _, want := range []string{
		"/var/lib/docker/volumes/app-data/_data",
		"/srv/binds/app-config",
	} {
		if !got[want] {
			t.Errorf("spec.Paths = %v, want to contain %q", spec.Paths, want)
		}
	}
}

// writeTempConfig writes contents to a ballast.yml under t.TempDir and
// returns its path.
func writeTempConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/ballast.yml"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}
