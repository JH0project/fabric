// Package app owns observation lifecycle and the local materialized view.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/JH0project/fabric/control/internal/runtime"
)

type App struct {
	Runtime         runtime.Runtime
	View            *runtime.View
	Logger          *slog.Logger
	RequestTimeout  time.Duration
	RefreshInterval time.Duration
	Tracer          trace.Tracer
	Meter           metric.Meter
	telemetry       *instruments
}

func (a *App) Run(ctx context.Context) error {
	if a.Runtime == nil || a.View == nil || a.Logger == nil || a.RequestTimeout <= 0 || a.RefreshInterval <= 0 {
		return errors.New("invalid app dependencies or observation intervals")
	}
	if ctx.Err() != nil {
		return nil
	}
	if a.Tracer == nil {
		a.Tracer = otel.Tracer("fabric/control/runtime")
	}
	if a.Meter == nil {
		a.Meter = otel.Meter("fabric/control/runtime")
	}
	registration, err := a.instruments(a.Meter)
	if err != nil {
		return fmt.Errorf("runtime instruments: %w", err)
	}
	defer registration.Unregister()
	// Replay from before the list, so events during bootstrap are included.
	since := time.Now()
	if err := a.refresh(ctx); err != nil {
		return shutdownError(ctx, err)
	}
	a.Logger.InfoContext(ctx, "runtime snapshot loaded", "fabric.runtime.containers", len(a.View.Snapshot()))

	watchCtx, cancel := context.WithCancel(ctx)
	events := make(chan runtime.Event)
	done := make(chan error, 1)
	go func() {
		done <- a.Runtime.Watch(watchCtx, since, func(event runtime.Event) error {
			select {
			case events <- event:
				return nil
			case <-watchCtx.Done():
				return watchCtx.Err()
			}
		})
	}()
	// Always cancel and join the watcher, including when list/inspect fails.
	defer func() { cancel(); <-done }()
	ticker := time.NewTicker(a.RefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			// Put the result back so deferred join also works on this path.
			done <- err
			if ctx.Err() != nil {
				return nil
			}
			if err == nil {
				err = errors.New("event stream ended")
			}
			return fmt.Errorf("watch runtime: %w", err)
		case <-ticker.C:
			// Docker retains only a bounded event history. Periodic observation
			// repairs missed invalidations without creating desired state.
			if err := a.refresh(ctx); err != nil {
				return shutdownError(ctx, err)
			}
		case event := <-events:
			a.telemetry.events.Add(ctx, 1, metric.WithAttributes(attribute.String("action", actionLabel(event.Action))))
			if err := a.observe(ctx, event); err != nil {
				return shutdownError(ctx, err)
			}
		}
	}
}

func (a *App) refresh(ctx context.Context) (err error) {
	ctx, span := a.Tracer.Start(ctx, "docker.snapshot")
	start := time.Now()
	defer func() { complete(ctx, span, start, err, a.telemetry.snapshots, a.telemetry.snapshotDuration) }()
	requestCtx, cancel := context.WithTimeout(ctx, a.RequestTimeout)
	defer cancel()
	containers, err := a.Runtime.List(requestCtx)
	if err != nil {
		return fmt.Errorf("snapshot runtime: %w", err)
	}
	previous := a.View.Snapshot()
	for _, c := range containers {
		a.logChange(ctx, c)
	}
	a.View.Replace(containers)
	present := make(map[string]bool, len(containers))
	for _, c := range containers {
		present[c.ID] = true
	}
	for _, c := range previous {
		if !present[c.ID] {
			a.Logger.InfoContext(ctx, "container removed", "container.id", c.ID, "container.name", c.Name)
		}
	}
	a.Logger.DebugContext(ctx, "runtime snapshot refreshed", "fabric.runtime.containers", len(containers))
	return nil
}

func (a *App) observe(ctx context.Context, event runtime.Event) (err error) {
	ctx, span := a.Tracer.Start(ctx, "docker.inspect", trace.WithAttributes(attribute.String("container.id", event.ContainerID), attribute.String("fabric.runtime.event.action", event.Action)))
	start := time.Now()
	defer func() { complete(ctx, span, start, err, a.telemetry.inspects, a.telemetry.inspectDuration) }()
	requestCtx, cancel := context.WithTimeout(ctx, a.RequestTimeout)
	defer cancel()
	c, err := a.Runtime.Inspect(requestCtx, event.ContainerID)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		span.SetAttributes(attribute.Bool("fabric.runtime.container.found", false))
		if old, ok := a.View.Get(event.ContainerID); ok {
			a.Logger.InfoContext(ctx, "container removed", "container.id", old.ID, "container.name", old.Name)
		}
		a.View.Remove(event.ContainerID)
		err = nil // expected deletion is a successful observation
	case err != nil:
		return fmt.Errorf("inspect container %s: %w", event.ContainerID, err)
	default:
		a.logChange(ctx, c)
		a.View.Upsert(c)
	}
	a.Logger.DebugContext(ctx, "runtime event observed", "container.id", event.ContainerID, "fabric.runtime.event.action", event.Action)
	return nil
}

func (a *App) logChange(ctx context.Context, c runtime.Container) {
	old, ok := a.View.Get(c.ID)
	if ok && old.State == c.State && old.Name == c.Name && old.Image == c.Image && maps.Equal(old.Labels, c.Labels) {
		return
	}
	a.Logger.InfoContext(ctx, "container state changed", "container.id", c.ID, "container.name", c.Name, "container.image.name", c.Image, "container.state", c.State)
}

func shutdownError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}
