// SPDX-License-Identifier: GPL-3.0-or-later

// Package runtime abstracts a container runtime (Docker or Podman) behind the
// small set of operations Ballast needs: discover containers and their mounts,
// watch the socket for lifecycle changes, exec into a container to quiesce or
// dump it, and stop or start it for a cold backup.
//
// The interface is deliberately kept free of any Ballast-specific type so it can
// be lifted into github.com/tagwright/core once a second suite tool consumes it.
// Until then it lives here and is shaped against exactly one real consumer, which
// is the discipline that keeps the abstraction honest.
//
// The Docker adapter lands first. A Podman adapter follows behind the same
// interface, absorbing the socket-path, rootless-stats, and journald differences.
package runtime

import (
	"context"
	"errors"
	"io"
)

// ErrNotImplemented is returned by adapter methods that are not wired up yet.
var ErrNotImplemented = errors.New("runtime: not implemented")

// Runtime is a container runtime Ballast can drive. Implementations must be safe
// for concurrent use by multiple goroutines.
type Runtime interface {
	// List returns every container the runtime knows about, running or not.
	List(ctx context.Context) ([]Container, error)

	// Inspect returns a single container by ID or name.
	Inspect(ctx context.Context, id string) (Container, error)

	// Watch streams lifecycle events until ctx is cancelled. The error channel
	// carries a terminal error and is then closed alongside the event channel.
	Watch(ctx context.Context) (<-chan Event, <-chan error)

	// Exec runs a command inside a running container and returns a handle whose
	// Stdout the caller reads (for stream backups it is piped straight into the
	// engine's stdin) before calling Wait for the exit code.
	Exec(ctx context.Context, id string, spec ExecSpec) (*ExecHandle, error)

	// Stop stops a running container, waiting up to timeoutSeconds before a kill.
	Stop(ctx context.Context, id string, timeoutSeconds int) error

	// Start starts a stopped container.
	Start(ctx context.Context, id string) error

	// Close releases the underlying client.
	Close() error
}

// Container is the normalized view of a container across runtimes.
type Container struct {
	ID      string
	Name    string
	State   string // running, exited, paused, ...
	Labels  map[string]string
	Mounts  []Mount
	Project string // com.docker.compose.project, empty if not a compose service
	Service string // com.docker.compose.service, empty if not a compose service
}

// MountType distinguishes the kinds of mount Ballast cares about.
type MountType string

const (
	MountVolume MountType = "volume"
	MountBind   MountType = "bind"
	MountTmpfs  MountType = "tmpfs"
)

// Mount is one filesystem mount attached to a container.
type Mount struct {
	Type        MountType
	Name        string // named-volume name, empty for binds and tmpfs
	Source      string // host-side path, empty for tmpfs
	Destination string // container-side path
	ReadOnly    bool
}

// EventType is a container lifecycle transition.
type EventType string

const (
	EventStart   EventType = "start"
	EventStop    EventType = "stop"
	EventDie     EventType = "die"
	EventDestroy EventType = "destroy"
)

// Event is a single lifecycle change on the socket.
type Event struct {
	Type   EventType
	ID     string
	Name   string
	Labels map[string]string
}

// ExecSpec describes a command to run inside a container.
type ExecSpec struct {
	Cmd  []string
	User string // empty means the container's default user
}

// ExecHandle is a running exec. The caller reads Stdout to completion, then
// calls Wait to learn the exit code. Stderr is captured separately for logging.
type ExecHandle struct {
	Stdout io.Reader
	Wait   func() (exitCode int, err error)
}
