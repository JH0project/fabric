// Package app owns observation lifecycle and the local materialized view.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/H01001000/jh0project-fabric/control/internal/runtime"
)

type App struct {
	Runtime         runtime.Runtime
	View            *runtime.View
	Logger          *slog.Logger
	RequestTimeout  time.Duration
	RefreshInterval time.Duration
}

func (a *App) Run(ctx context.Context) error {
	if a.Runtime == nil || a.View == nil || a.Logger == nil || a.RequestTimeout <= 0 || a.RefreshInterval <= 0 {
		return errors.New("invalid app dependencies or observation intervals")
	}
	if ctx.Err() != nil {
		return nil
	}
	// Replay from before the list, so events during bootstrap are included.
	since := time.Now()
	if err := a.refresh(ctx); err != nil {
		return shutdownError(ctx, err)
	}
	a.Logger.Info("runtime snapshot loaded", "containers", len(a.View.Snapshot()))

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
			if err := a.observe(ctx, event); err != nil {
				return shutdownError(ctx, err)
			}
		}
	}
}

func (a *App) refresh(ctx context.Context) error {
	requestCtx, cancel := context.WithTimeout(ctx, a.RequestTimeout)
	defer cancel()
	containers, err := a.Runtime.List(requestCtx)
	if err != nil {
		return fmt.Errorf("snapshot runtime: %w", err)
	}
	a.View.Replace(containers)
	return nil
}

func (a *App) observe(ctx context.Context, event runtime.Event) error {
	requestCtx, cancel := context.WithTimeout(ctx, a.RequestTimeout)
	defer cancel()
	c, err := a.Runtime.Inspect(requestCtx, event.ContainerID)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		a.View.Remove(event.ContainerID)
	case err != nil:
		return fmt.Errorf("inspect container %s: %w", event.ContainerID, err)
	default:
		a.View.Upsert(c)
	}
	a.Logger.Debug("runtime view updated", "container", event.ContainerID, "action", event.Action)
	return nil
}

func shutdownError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return nil
	}
	return err
}
