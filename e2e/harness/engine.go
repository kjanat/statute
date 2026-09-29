//go:build e2e

package harness

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// ContainerSpec describes one scenario-owned container.
type ContainerSpec struct {
	Name       string
	Image      string
	Entrypoint []string
	Env        []string
	Labels     map[string]string
	Network    string
	Cmd        []string
}

// ContainerState contains lifecycle observations without application readiness.
type ContainerState struct {
	Running  bool
	ExitCode int
}

// Engine shares one client and tracks immutable IDs created by a scenario.
type Engine struct {
	api     *client.Client
	host    string
	project string
	mu      sync.Mutex
	owned   map[string]struct{}
}

// NewEngine resolves the supported Docker endpoint before acquiring resources.
func NewEngine(ctx context.Context, project string) (*Engine, error) {
	host, err := resolveDaemon(ctx)
	if err != nil {
		return nil, err
	}
	api, err := client.New(client.WithHost(host))
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}
	return &Engine{api: api, host: host, project: project, owned: make(map[string]struct{})}, nil
}

// Host returns the frozen daemon endpoint shared with Compose.
func (e *Engine) Host() string { return e.host }

// Close releases client connections after cleanup completes.
func (e *Engine) Close() error { return e.api.Close() }

// Create registers ownership before the caller can start the container.
func (e *Engine) Create(ctx context.Context, spec ContainerSpec) (string, error) {
	if e.project == "" {
		return "", fmt.Errorf("create container %q: a project owner is required", spec.Name)
	}
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	labels := maps.Clone(spec.Labels)
	if labels == nil {
		labels = make(map[string]string)
	}
	labels["statute.e2e"] = "1"
	labels["com.docker.compose.project"] = e.project
	if labels["statute.enable"] != "true" {
		labels["statute.enable"] = "false"
	}
	result, err := e.api.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       spec.Name,
		Config:     &container.Config{Image: spec.Image, Entrypoint: spec.Entrypoint, Cmd: spec.Cmd, Env: spec.Env, Labels: labels},
		HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(spec.Network)},
	})
	if err != nil {
		return "", fmt.Errorf("create container %q: %w", spec.Name, err)
	}
	e.mu.Lock()
	e.owned[result.ID] = struct{}{}
	e.mu.Unlock()
	return result.ID, nil
}

// Start starts an immutable container ID; errors retain cleanup ownership.
func (e *Engine) Start(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	_, err := e.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return operationError("start", id, err)
}

// Stop stops a container with a bounded graceful shutdown.
func (e *Engine) Stop(ctx context.Context, id string) error { return e.stop(ctx, id, 10*time.Second) }

func (e *Engine) stop(ctx context.Context, id string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	seconds := int(timeout.Seconds())
	_, err := e.api.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &seconds})
	return operationError("stop", id, err)
}

// Remove force-removes a container and releases ownership only after confirmation.
func (e *Engine) Remove(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	_, err := e.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	if err != nil && !errors.Is(err, errdefs.ErrNotFound) {
		return operationError("remove", id, err)
	}
	e.mu.Lock()
	delete(e.owned, id)
	e.mu.Unlock()
	return nil
}

// Inspect returns typed lifecycle state from the daemon.
func (e *Engine) Inspect(ctx context.Context, id string) (ContainerState, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	out, err := e.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return ContainerState{}, operationError("inspect", id, err)
	}
	if out.Container.State == nil {
		return ContainerState{}, fmt.Errorf("inspect container %s: missing state", id)
	}
	return ContainerState{Running: out.Container.State.Running, ExitCode: out.Container.State.ExitCode}, nil
}

// Logs returns demultiplexed stdout and stderr from a non-TTY scenario container.
func (e *Engine) Logs(ctx context.Context, id string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	stream, err := e.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true})
	if err != nil {
		return "", operationError("logs", id, err)
	}
	var out bytes.Buffer
	_, err = stdcopy.StdCopy(&out, &out, stream)
	return out.String(), errors.Join(operationError("read logs", id, err), operationError("close logs", id, stream.Close()))
}

// Wait waits for stopped state and returns the process exit code.
func (e *Engine) Wait(ctx context.Context, id string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	wait := e.api.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case result := <-wait.Result:
		if result.Error != nil {
			return 0, fmt.Errorf("wait container %s: %s", id, result.Error.Message)
		}
		return int(result.StatusCode), nil
	case err := <-wait.Error:
		return 0, operationError("wait", id, err)
	}
}

func (e *Engine) ownedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.owned))
	for id := range e.owned {
		ids = append(ids, id)
	}
	return ids
}

// Cleanup attempts every tracked removal and reports all failures.
func (e *Engine) Cleanup(ctx context.Context) error {
	ids := e.ownedIDs()
	errs := make([]error, 0, len(ids))
	for _, id := range ids {
		errs = append(errs, e.Remove(ctx, id))
	}
	return errors.Join(errs...)
}

func operationError(op, id string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s container %s: %w", op, id, err)
}
