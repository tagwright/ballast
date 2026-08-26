// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
)

// Compose label keys Podman-aware compose tooling attaches to a container.
// podman-compose (and podman's native "podman compose" wrapper, which
// shells out to podman-compose or docker-compose) sets both the
// Docker-compatible com.docker.compose.* pair defined in docker.go and this
// Podman-native io.podman.compose.* pair on every container it creates.
// This pair is kept only as a fallback for tooling that sets solely the
// Podman-native labels (older podman-compose releases, or containers
// started some other way that only sets these).
const (
	podmanComposeProjectLabel = "io.podman.compose.project"
	podmanComposeServiceLabel = "io.podman.compose.service"
)

// defaultRootfulPodmanSocket is the conventional socket path for a rootful
// Podman system service (podman.socket enabled at the system level).
const defaultRootfulPodmanSocket = "/run/podman/podman.sock"

// PodmanRuntime is the Podman adapter for Runtime. Podman's REST API
// includes a Docker-compatible compat layer (documented against the Docker
// v1.40 API) on the same socket as its native libpod API, so PodmanRuntime
// talks to it with the exact request and mapping machinery DockerRuntime
// uses, in engine.go; the two adapters differ only in their default socket
// path and in how compose project/service identity is read off a
// container's labels.
//
// The client is created lazily on first use and cached, so constructing a
// PodmanRuntime never touches the socket: nothing fails until a method that
// actually needs the daemon is called.
type PodmanRuntime struct {
	*engineClient
}

// NewPodman returns a Podman adapter bound to the given API socket path. An
// empty socket resolves to a sensible default: the rootless per-user socket
// derived from XDG_RUNTIME_DIR (or /run/user/<uid> when that is unset,
// matching systemd's own convention) for a non-root caller, and the rootful
// system-service socket for a root caller with no XDG_RUNTIME_DIR set.
func NewPodman(socket string) *PodmanRuntime {
	if socket == "" {
		socket = defaultPodmanSocket()
	}
	return &PodmanRuntime{engineClient: &engineClient{
		engine:   "podman",
		socket:   socket,
		identity: podmanComposeIdentity,
	}}
}

// compile-time assertion that the adapter satisfies the interface.
var _ Runtime = (*PodmanRuntime)(nil)

// defaultPodmanSocket returns the conventional Podman API socket path for
// the process's own privilege level, matching the defaults podman-remote
// itself documents: the rootless per-user socket when running unprivileged,
// or the rootful system-service socket when running as root with no
// XDG_RUNTIME_DIR in the environment.
func defaultPodmanSocket() string {
	if runtimeDir := os.Getenv("XDG_RUNTIME_DIR"); runtimeDir != "" {
		return filepath.Join(runtimeDir, "podman", "podman.sock")
	}
	if uid := os.Getuid(); uid != 0 {
		return fmt.Sprintf("/run/user/%d/podman/podman.sock", uid)
	}
	return defaultRootfulPodmanSocket
}

// podmanComposeIdentity resolves compose project/service names from a
// Podman-managed container's labels. The Docker-compatible
// com.docker.compose.* pair is preferred, since podman-compose sets it
// alongside its own io.podman.compose.* pair and the rest of Ballast
// already expects that label vocabulary; the io.podman.compose.* pair is
// used only as a fallback when the compat labels are absent.
func podmanComposeIdentity(labels map[string]string) (project, service string) {
	if project, service = labels[composeProjectLabel], labels[composeServiceLabel]; service != "" {
		return project, service
	}
	return labels[podmanComposeProjectLabel], labels[podmanComposeServiceLabel]
}
