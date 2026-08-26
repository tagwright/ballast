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

// composeIdentityFunc resolves a container's compose project and service
// names from its labels. Docker and Podman disagree on which label keys
// carry that grouping (Podman's own tooling adds an io.podman.compose.*
// pair alongside, or instead of, Docker's com.docker.compose.*), so each
// engineClient is handed the mapping its runtime needs rather than the
// shared machinery hard-coding one label set.
type composeIdentityFunc func(labels map[string]string) (project, service string)

// engineClient is the request and mapping machinery shared by every adapter
// that talks to a Docker Engine API-compatible socket. DockerRuntime and
// PodmanRuntime both embed one; the only things that differ between the two
// engines are the socket path and how compose project/service identity is
// read off a container's labels, both supplied at construction time.
//
// The client is created lazily on first use and cached, so constructing an
// engineClient never touches the socket: nothing fails until a method that
// actually needs the daemon is called.
type engineClient struct {
	// engine names the runtime for error messages, e.g. "docker" or
	// "podman".
	engine string

	// socket is the path to the API socket, e.g. /var/run/docker.sock or
	// /run/podman/podman.sock.
	socket string

	// identity resolves compose project/service names from a container's
	// labels.
	identity composeIdentityFunc

	mu     sync.Mutex
	client *client.Client
}

// clientFor returns the cached engine API client, creating it on first call.
// API version negotiation means the client adapts to whatever the daemon on
// the other end of the socket speaks, rather than pinning a version Ballast
// has to keep in lockstep with the engine.
func (e *engineClient) clientFor() (*client.Client, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client != nil {
		return e.client, nil
	}

	cli, err := client.NewClientWithOpts(
		client.WithHost("unix://"+e.socket),
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: new client: %w", e.engine, err)
	}
	e.client = cli
	return cli, nil
}

// List returns every container the runtime knows about, running or not.
func (e *engineClient) List(ctx context.Context) ([]Container, error) {
	cli, err := e.clientFor()
	if err != nil {
		return nil, err
	}

	summaries, err := cli.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: list containers: %w", e.engine, err)
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

		project, service := e.identity(s.Labels)
		out = append(out, Container{
			ID:      s.ID,
			Name:    name,
			State:   s.State,
			Labels:  s.Labels,
			Mounts:  mounts,
			Project: project,
			Service: service,
		})
	}
	return out, nil
}

// Inspect returns a single container by ID or name.
func (e *engineClient) Inspect(ctx context.Context, id string) (Container, error) {
	cli, err := e.clientFor()
	if err != nil {
		return Container{}, err
	}

	info, err := cli.ContainerInspect(ctx, id)
	if err != nil {
		return Container{}, fmt.Errorf("runtime/%s: inspect container %s: %w", e.engine, id, err)
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

	project, service := e.identity(labels)
	return Container{
		ID:      info.ID,
		Name:    strings.TrimPrefix(info.Name, "/"),
		State:   state,
		Labels:  labels,
		Mounts:  mounts,
		Project: project,
		Service: service,
	}, nil
}

// mapMountPoint translates a Docker Engine API mount point into Ballast's
// normalized Mount type. Anything the engine reports that is not a
// recognized bind or tmpfs mount is treated as a named volume, which is the
// common case and the one Ballast most needs to get right (it is what gets
// dumped or archived). Podman's compat API reports mounts in the same
// shape, so this mapping is engine-agnostic.
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

// mapEventAction translates a Docker Engine API event action into Ballast's
// normalized EventType. Actions Ballast does not act on (health checks,
// exec, resize, and the like) are reported as not-ok so the caller can skip
// them.
//
// Podman's compat event stream reuses most of the same action vocabulary,
// but not all of it: where a real Docker daemon emits "destroy" for a
// container's removal, Podman's compat API (confirmed against a live
// Podman 5.8 socket, not merely its documentation) emits "remove" instead,
// and never emits "destroy" for a container at all. Both are mapped to
// EventDestroy here so daemon/watch.go's die-or-destroy unregistration
// fires correctly on both runtimes; a real Docker daemon has never been
// observed to emit ActionRemove for a container (only for other resource
// types), so widening the match costs Docker nothing.
func mapEventAction(action events.Action) (EventType, bool) {
	switch action {
	case events.ActionStart:
		return EventStart, true
	case events.ActionStop:
		return EventStop, true
	case events.ActionDie:
		return EventDie, true
	case events.ActionDestroy, events.ActionRemove:
		return EventDestroy, true
	default:
		return "", false
	}
}

// Watch streams lifecycle events until ctx is cancelled. The error channel
// carries a terminal error and is then closed alongside the event channel.
func (e *engineClient) Watch(ctx context.Context) (<-chan Event, <-chan error) {
	out := make(chan Event)
	errs := make(chan error, 1)

	cli, err := e.clientFor()
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
func (e *engineClient) Exec(ctx context.Context, id string, spec ExecSpec) (*ExecHandle, error) {
	cli, err := e.clientFor()
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
		return nil, fmt.Errorf("runtime/%s: exec create on %s: %w", e.engine, id, err)
	}

	attach, err := cli.ContainerExecAttach(ctx, created.ID, container.ExecAttachOptions{})
	if err != nil {
		return nil, fmt.Errorf("runtime/%s: exec attach on %s: %w", e.engine, id, err)
	}

	stdoutR, stdoutW := io.Pipe()
	var stderrBuf bytes.Buffer
	done := make(chan struct{})

	// The engine multiplexes stdout and stderr onto one stream when the exec
	// was not created with a TTY. stdcopy demultiplexes it as it arrives, so
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
			return 0, fmt.Errorf("runtime/%s: exec inspect on %s: %w", e.engine, id, err)
		}

		if inspect.ExitCode != 0 {
			return inspect.ExitCode, fmt.Errorf("runtime/%s: exec %v on %s exited %d: %s",
				e.engine, cmd, id, inspect.ExitCode, strings.TrimSpace(stderrBuf.String()))
		}
		return inspect.ExitCode, nil
	}

	return &ExecHandle{Stdout: stdoutR, Wait: wait}, nil
}

// Stop stops a running container, waiting up to timeoutSeconds before a kill.
func (e *engineClient) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	timeout := timeoutSeconds
	if err := cli.ContainerStop(ctx, id, container.StopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("runtime/%s: stop %s: %w", e.engine, id, err)
	}
	return nil
}

// Start starts a stopped container.
func (e *engineClient) Start(ctx context.Context, id string) error {
	cli, err := e.clientFor()
	if err != nil {
		return err
	}

	if err := cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		return fmt.Errorf("runtime/%s: start %s: %w", e.engine, id, err)
	}
	return nil
}

// Close releases the underlying client.
func (e *engineClient) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.client == nil {
		return nil
	}
	err := e.client.Close()
	e.client = nil
	return err
}
