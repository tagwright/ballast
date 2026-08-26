// SPDX-License-Identifier: GPL-3.0-or-later

package runtime

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

// Compose label keys Docker attaches to a container that a compose file
// brought up. Their presence (and value) is how Ballast recovers the
// project/service grouping without parsing any compose YAML itself.
const (
	composeProjectLabel = "com.docker.compose.project"
	composeServiceLabel = "com.docker.compose.service"
)

// DockerRuntime is the Docker adapter for Runtime. It talks to the Docker Engine
// API over the socket Ballast mounts read-only.
//
// The client is created lazily on first use and cached, so constructing a
// DockerRuntime never touches the socket: nothing fails until a method that
// actually needs the daemon is called.
type DockerRuntime struct {
	// socket is the path to the Docker API socket, e.g. /var/run/docker.sock.
	socket string

	mu     sync.Mutex
	client *client.Client
}

// NewDocker returns a Docker adapter bound to the given API socket path.
func NewDocker(socket string) *DockerRuntime {
	return &DockerRuntime{socket: socket}
}

// compile-time assertion that the adapter satisfies the interface.
var _ Runtime = (*DockerRuntime)(nil)

// clientFor returns the cached engine API client, creating it on first call.
// API version negotiation means the client adapts to whatever the daemon on
// the other end of the socket speaks, rather than pinning a version Ballast
// has to keep in lockstep with the engine.
func (d *DockerRuntime) clientFor() (*client.Client, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.client != nil {
		return d.client, nil
	}

	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+d.socket),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("runtime/docker: new client: %w", err)
	}
	d.client = cli
	return cli, nil
}

// List returns every container the runtime knows about, running or not.
func (d *DockerRuntime) List(ctx context.Context) ([]Container, error) {
	cli, err := d.clientFor()
	if err != nil {
		return nil, err
	}

	summaries, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("runtime/docker: list containers: %w", err)
	}

	out := make([]Container, 0, len(summaries))
	for _, s := range summaries {
		name := ""
		if len(s.Names) > 0 {
			name = strings.TrimPrefix(s.Names[0], "/")
		}

		mounts := make([]Mount, 0, len(s.Mounts))
		for _, m := range s.Mounts {
			mounts = append(mounts, mapMountPoint(m))
		}

		out = append(out, Container{
			ID:      s.ID,
			Name:    name,
			State:   s.State,
			Labels:  s.Labels,
			Mounts:  mounts,
			Project: s.Labels[composeProjectLabel],
			Service: s.Labels[composeServiceLabel],
		})
	}
	return out, nil
}

// Inspect returns a single container by ID or name.
func (d *DockerRuntime) Inspect(ctx context.Context, id string) (Container, error) {
	cli, err := d.clientFor()
	if err != nil {
		return Container{}, err
	}

	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return Container{}, fmt.Errorf("runtime/docker: inspect container %s: %w", id, err)
	}

	var labels map[string]string
	if info.Config != nil {
		labels = info.Config.Labels
	}

	state := ""
	if info.State != nil {
		state = info.State.Status
	}

	mounts := make([]Mount, 0, len(info.Mounts))
	for _, m := range info.Mounts {
		mounts = append(mounts, mapMountPoint(m))
	}

	return Container{
		ID:      info.ID,
		Name:    strings.TrimPrefix(info.Name, "/"),
		State:   state,
		Labels:  labels,
		Mounts:  mounts,
		Project: labels[composeProjectLabel],
		Service: labels[composeServiceLabel],
	}, nil
}

// mapMountPoint translates a Docker mount point into Ballast's normalized
// Mount type. Anything the engine reports that is not a recognized bind or
// tmpfs mount is treated as a named volume, which is the common case and the
// one Ballast most needs to get right (it is what gets dumped or archived).
func mapMountPoint(m container.MountPoint) Mount {
	mt := MountVolume
	switch m.Type {
	case mount.TypeBind:
		mt = MountBind
	case mount.TypeTmpfs:
		mt = MountTmpfs
	case mount.TypeVolume:
		mt = MountVolume
	}

	return Mount{
		Type:        mt,
		Name:        m.Name,
		Source:      m.Source,
		Destination: m.Destination,
		ReadOnly:    !m.RW,
	}
}

// mapEventAction translates a Docker event action into Ballast's normalized
// EventType. Actions Ballast does not act on (health checks, exec, resize,
// and the like) are reported as not-ok so the caller can skip them.
func mapEventAction(action events.Action) (EventType, bool) {
	switch action {
	case events.ActionStart:
		return EventStart, true
	case events.ActionStop:
		return EventStop, true
	case events.ActionDie:
		return EventDie, true
	case events.ActionDestroy:
		return EventDestroy, true
	default:
		return "", false
	}
}

// Watch streams lifecycle events until ctx is cancelled. The error channel
// carries a terminal error and is then closed alongside the event channel.
func (d *DockerRuntime) Watch(ctx context.Context) (<-chan Event, <-chan error) {
	out := make(chan Event)
	errs := make(chan error, 1)

	cli, err := d.clientFor()
	if err != nil {
		errs <- err
		close(out)
		close(errs)
		return out, errs
	}

	msgs, errCh := cli.Events(ctx, events.ListOptions{
		Filters: filters.NewArgs(filters.Arg("type", string(events.ContainerEventType))),
	})

	go func() {
		defer close(out)
		defer close(errs)

		for {
			select {
			case <-ctx.Done():
				return

			case err, ok := <-errCh:
				if !ok {
					return
				}
				if err != nil {
					errs <- err
				}
				return

			case msg, ok := <-msgs:
				if !ok {
					return
				}
				et, ok := mapEventAction(msg.Action)
				if !ok {
					continue
				}
				out <- Event{
					Type:   et,
					ID:     msg.Actor.ID,
					Name:   msg.Actor.Attributes["name"],
					Labels: msg.Actor.Attributes,
				}
			}
		}
	}()

	return out, errs
}

// Exec runs a command inside a running container and returns a handle whose
// Stdout streams the command's standard output as it is produced, which
// matters because the caller pipes a live dump into restic --stdin rather
// than buffering it. Standard error is captured separately and folded into
// the error Wait returns on a non-zero exit.
func (d *DockerRuntime) Exec(ctx context.Context, id string, spec ExecSpec) (*ExecHandle, error) {
	cli, err := d.clientFor()
	if err != nil {
		return nil, err
	}

	created, err := cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd:          spec.Cmd,
		User:         spec.User,
		AttachStdout: true,
		AttachStderr: true,
	})
	if err != nil {
		return nil, fmt.Errorf("runtime/docker: exec create on %s: %w", id, err)
	}

	attach, err := cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("runtime/docker: exec attach on %s: %w", id, err)
	}

	stdoutR, stdoutW := io.Pipe()
	var stderrBuf bytes.Buffer
	done := make(chan struct{})

	// Docker multiplexes stdout and stderr onto one stream when the exec was
	// not created with a TTY. stdcopy demultiplexes it as it arrives, so
	// stdout keeps flowing to the pipe reader instead of waiting for the
	// command to finish.
	go func() {
		defer close(done)
		defer attach.Close()
		_, copyErr := stdcopy.StdCopy(stdoutW, &stderrBuf, attach.Reader)
		stdoutW.CloseWithError(copyErr)
	}()

	cmd := spec.Cmd
	wait := func() (int, error) {
		<-done

		inspect, err := cli.ContainerExecInspect(ctx, created.ID)
		if err != nil {
			return 0, fmt.Errorf("runtime/docker: exec inspect on %s: %w", id, err)
		}

		if inspect.ExitCode != 0 {
			return inspect.ExitCode, fmt.Errorf("runtime/docker: exec %v on %s exited %d: %s",
				cmd, id, inspect.ExitCode, strings.TrimSpace(stderrBuf.String()))
		}
		return inspect.ExitCode, nil
	}

	return &ExecHandle{Stdout: stdoutR, Wait: wait}, nil
}

// Stop stops a running container, waiting up to timeoutSeconds before a kill.
func (d *DockerRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	cli, err := d.clientFor()
	if err != nil {
		return err
	}

	timeout := timeoutSeconds
	if err := cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("runtime/docker: stop %s: %w", id, err)
	}
	return nil
}

// Start starts a stopped container.
func (d *DockerRuntime) Start(ctx context.Context, id string) error {
	cli, err := d.clientFor()
	if err != nil {
		return err
	}

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("runtime/docker: start %s: %w", id, err)
	}
	return nil
}

// Close releases the underlying client.
func (d *DockerRuntime) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.client == nil {
		return nil
	}
	err := d.client.Close()
	d.client = nil
	return err
}
