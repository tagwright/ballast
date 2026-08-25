package runtime

import "context"

// DockerRuntime is the Docker adapter for Runtime. It talks to the Docker Engine
// API over the socket Ballast mounts read-only.
//
// The method bodies are stubs for now: the interface, the normalized types, and
// the wiring seam are settled, and the Docker SDK calls land in the next pass
// (which pulls in the engine API client and moves go.mod off the standard
// library). Keeping the stub here lets the rest of the tree compile and build
// against a stable shape.
type DockerRuntime struct {
	// socket is the path to the Docker API socket, e.g. /var/run/docker.sock.
	socket string
}

// NewDocker returns a Docker adapter bound to the given API socket path.
func NewDocker(socket string) *DockerRuntime {
	return &DockerRuntime{socket: socket}
}

// compile-time assertion that the adapter satisfies the interface.
var _ Runtime = (*DockerRuntime)(nil)

func (d *DockerRuntime) List(ctx context.Context) ([]Container, error) {
	return nil, ErrNotImplemented
}

func (d *DockerRuntime) Inspect(ctx context.Context, id string) (Container, error) {
	return Container{}, ErrNotImplemented
}

func (d *DockerRuntime) Watch(ctx context.Context) (<-chan Event, <-chan error) {
	events := make(chan Event)
	errs := make(chan error, 1)
	errs <- ErrNotImplemented
	close(events)
	close(errs)
	return events, errs
}

func (d *DockerRuntime) Exec(ctx context.Context, id string, spec ExecSpec) (*ExecHandle, error) {
	return nil, ErrNotImplemented
}

func (d *DockerRuntime) Stop(ctx context.Context, id string, timeoutSeconds int) error {
	return ErrNotImplemented
}

func (d *DockerRuntime) Start(ctx context.Context, id string) error {
	return ErrNotImplemented
}

func (d *DockerRuntime) Close() error {
	return nil
}
