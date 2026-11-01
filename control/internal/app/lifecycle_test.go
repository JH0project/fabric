package app

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type component struct {
	name string
	run  func(context.Context) error
}

func (c component) Name() string                  { return c.name }
func (c component) Run(ctx context.Context) error { return c.run(ctx) }

func TestLifecycleFatalFailureCancelsAndJoins(t *testing.T) {
	boom := errors.New("fatal component failure")
	var joined atomic.Bool
	a := New(component{"observer", func(ctx context.Context) error { <-ctx.Done(); joined.Store(true); return ctx.Err() }}, component{"broken", func(context.Context) error { return boom }})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := a.Run(ctx)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "broken") || !joined.Load() {
		t.Fatalf("failure identity or joined shutdown lost: %v", err)
	}
}

func TestLifecycleParentCancellationIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	joined := make(chan struct{})
	a := New(component{"worker", func(ctx context.Context) error { close(ready); <-ctx.Done(); close(joined); return ctx.Err() }})
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	<-ready
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("application did not stop")
	}
	select {
	case <-joined:
	default:
		t.Fatal("component not joined")
	}
}

func TestLifecycleUnexpectedExitIsFatal(t *testing.T) {
	a := New(component{"returned", func(context.Context) error { return nil }})
	if err := a.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "returned before") {
		t.Fatal("silent daemon exit accepted", err)
	}
}

func TestLifecycleValidatesBeforeStarting(t *testing.T) {
	var started atomic.Bool
	c := component{"same", func(context.Context) error { started.Store(true); return nil }}
	var nilComponent *component
	for _, a := range []*App{New(), New(nil), New(nilComponent), New(c, c), New(component{"", c.run})} {
		if err := a.Run(context.Background()); err == nil {
			t.Fatal("invalid components accepted")
		}
	}
	if started.Load() {
		t.Fatal("component started before registration validation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(c).Run(ctx); err != nil || started.Load() {
		t.Fatal("cancelled bootstrap started components", err)
	}
}
