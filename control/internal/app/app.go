package app

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
)

// Component owns one long-running loop/server, including its child goroutines.
// Run blocks until cancellation or fatal failure and joins its children before
// returning. Recoverable retries belong inside the component.
type Component interface {
	Name() string
	Run(context.Context) error
}

// App owns concurrency and cancellation, not feature logic or client resources.
// Bootstrap constructs dependencies; their cleanup runs after App.Run joins.
type App struct{ components []Component }

func New(components ...Component) *App {
	return &App{components: append([]Component(nil), components...)}
}

func (a *App) Run(ctx context.Context) error {
	if len(a.components) == 0 {
		return errors.New("application requires at least one component")
	}
	names := make(map[string]bool, len(a.components))
	for _, c := range a.components {
		if c == nil || (reflect.ValueOf(c).Kind() == reflect.Ptr && reflect.ValueOf(c).IsNil()) {
			return errors.New("nil application component")
		}
		name := c.Name()
		if strings.TrimSpace(name) == "" || names[name] {
			return fmt.Errorf("invalid or duplicate component name %q", name)
		}
		names[name] = true
	}
	if ctx.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(a.components))
	for _, c := range a.components {
		go func() {
			err := c.Run(ctx)
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				err = nil
			}
			if err == nil && ctx.Err() == nil {
				err = errors.New("returned before application cancellation")
			}
			if err != nil {
				err = fmt.Errorf("component %s: %w", c.Name(), err)
			}
			done <- err
		}()
	}
	result := <-done
	cancel()
	for i := 1; i < len(a.components); i++ {
		result = errors.Join(result, <-done)
	}
	return result
}
