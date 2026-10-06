// Package runtime defines the small runtime boundary consumed by agentd.
package runtime

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("container not found")

type Container struct {
	ID     string
	Name   string
	Image  string
	State  string
	Labels map[string]string
}

// Event is an invalidation hint, not authoritative container state.
type Event struct {
	ContainerID string
	Action      string
}

type Runtime interface {
	List(context.Context) ([]Container, error)
	Inspect(context.Context, string) (Container, error)
	// Watch blocks until cancellation, a stream error, or a callback error.
	// The callback is serial; implementations must honor context cancellation.
	Watch(context.Context, time.Time, func(Event) error) error
}
