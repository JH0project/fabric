// Package docker is the only production package that imports the Docker SDK.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/JH0project/JH0project-fabric/control/internal/runtime"
)

type Runtime struct{ client *client.Client }

// New honors DOCKER_HOST, DOCKER_TLS_VERIFY, DOCKER_CERT_PATH and
// DOCKER_API_VERSION. API negotiation is enabled by default by the SDK.
func New() (*Runtime, error) {
	c, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("create Docker client: %w", err)
	}
	return &Runtime{client: c}, nil
}

func (r *Runtime) Close() error { return r.client.Close() }

func (r *Runtime) List(ctx context.Context) ([]runtime.Container, error) {
	result, err := r.client.ContainerList(ctx, client.ContainerListOptions{All: true})
	if err != nil {
		return nil, err
	}
	containers := make([]runtime.Container, 0, len(result.Items))
	for _, c := range result.Items {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		containers = append(containers, runtime.Container{
			ID: c.ID, Name: name, Image: c.Image, State: string(c.State), Labels: c.Labels,
		})
	}
	return containers, nil
}

func (r *Runtime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	result, err := r.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if errors.Is(err, errdefs.ErrNotFound) {
		return runtime.Container{}, runtime.ErrNotFound
	}
	if err != nil {
		return runtime.Container{}, err
	}
	c := result.Container
	if c.State == nil || c.Config == nil {
		return runtime.Container{}, errors.New("incomplete Docker container inspect response")
	}
	return runtime.Container{
		ID: c.ID, Name: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image,
		State: string(c.State.Status), Labels: c.Config.Labels,
	}, nil
}

func (r *Runtime) Watch(ctx context.Context, since time.Time, emit func(runtime.Event) error) error {
	watchCtx, cancel := context.WithCancel(ctx)
	stream := r.client.Events(watchCtx, client.EventsListOptions{
		Since:   since.UTC().Format(time.RFC3339Nano),
		Filters: client.Filters{}.Add("type", "container"),
	})
	// The SDK closes Err when its reader exits. Join that reader on every
	// path, including callback failure, before the client can be closed.
	defer func() {
		cancel()
		for range stream.Err {
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err, ok := <-stream.Err:
			if ctx.Err() != nil {
				return nil
			}
			if !ok || err == nil {
				return io.EOF
			}
			return err
		case message := <-stream.Messages:
			if message.Actor.ID == "" {
				continue
			}
			if err := emit(runtime.Event{ContainerID: message.Actor.ID, Action: string(message.Action)}); err != nil {
				return err
			}
		}
	}
}

var _ runtime.Runtime = (*Runtime)(nil)
