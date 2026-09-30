//go:build e2e

package harness

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

func (e *Engine) containerIDs(ctx context.Context, filters client.Filters) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	out, err := e.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	ids := make([]string, 0, len(out.Items))
	for _, item := range out.Items {
		ids = append(ids, item.ID)
	}
	return ids, nil
}

func (e *Engine) projectFilter() client.Filters {
	return make(client.Filters).Add("label", "com.docker.compose.project="+e.project)
}

func (e *Engine) serviceID(ctx context.Context, service string) (string, error) {
	ids, err := e.containerIDs(ctx, e.projectFilter().Add("label", "com.docker.compose.service="+service))
	if err != nil {
		return "", err
	}
	if len(ids) != 1 {
		return "", fmt.Errorf("service %s: expected one container, got %v", service, ids)
	}
	return ids[0], nil
}

func (e *Engine) signal(ctx context.Context, id, signal string) error {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	_, err := e.api.ContainerKill(ctx, id, client.ContainerKillOptions{Signal: signal})
	return operationError("signal", id, err)
}

func (e *Engine) restart(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	seconds := 20
	_, err := e.api.ContainerRestart(ctx, id, client.ContainerRestartOptions{Timeout: &seconds})
	return operationError("restart", id, err)
}

func (e *Engine) projectOrphans(ctx context.Context) ([]string, error) {
	ids, err := e.containerIDs(ctx, e.projectFilter())
	errs := make([]error, 0, len(ids)+3)
	errs = append(errs, err)
	for _, id := range ids {
		errs = append(errs, e.Remove(ctx, id))
	}
	networks, err := e.networkOrphans(ctx)
	errs = append(errs, err)
	volumes, err := e.volumeOrphans(ctx)
	errs = append(errs, err)
	found := make([]string, 0, len(ids)+len(networks)+len(volumes))
	for _, id := range ids {
		found = append(found, "container:"+id)
	}
	found = append(found, networks...)
	found = append(found, volumes...)
	return found, errors.Join(errs...)
}

func (e *Engine) networkOrphans(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	out, err := e.api.NetworkList(ctx, client.NetworkListOptions{Filters: e.projectFilter()})
	if err != nil {
		return nil, fmt.Errorf("list project networks: %w", err)
	}
	var ids []string
	var errs []error
	for _, item := range out.Items {
		ids = append(ids, "network:"+item.ID)
		_, err := e.api.NetworkRemove(ctx, item.ID, client.NetworkRemoveOptions{})
		if err != nil && !errors.Is(err, errdefs.ErrNotFound) {
			errs = append(errs, fmt.Errorf("remove network %s: %w", item.ID, err))
		}
	}
	return ids, errors.Join(errs...)
}

func (e *Engine) volumeOrphans(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, composeTimeout)
	defer cancel()
	out, err := e.api.VolumeList(ctx, client.VolumeListOptions{Filters: e.projectFilter()})
	if err != nil {
		return nil, fmt.Errorf("list project volumes: %w", err)
	}
	var ids []string
	var errs []error
	for _, item := range out.Items {
		ids = append(ids, "volume:"+item.Name)
		_, err := e.api.VolumeRemove(ctx, item.Name, client.VolumeRemoveOptions{Force: true})
		if err != nil && !errors.Is(err, errdefs.ErrNotFound) {
			errs = append(errs, fmt.Errorf("remove volume %s: %w", item.Name, err))
		}
	}
	return ids, errors.Join(errs...)
}

// SweepLaneOrphans reaps lane-labeled containers and reports inspection failures.
func SweepLaneOrphans(ctx context.Context) (ids []string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	engine, err := NewEngine(ctx, "")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, engine.Close()) }()
	ids, err = engine.containerIDs(ctx, make(client.Filters).Add("label", "statute.e2e=1"))
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		err = errors.Join(err, engine.Remove(ctx, id))
	}
	return ids, err
}
