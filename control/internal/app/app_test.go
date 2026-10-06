package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/runtime"
)

type fakeRuntime struct {
	list    func(context.Context) ([]runtime.Container, error)
	inspect func(context.Context, string) (runtime.Container, error)
	watch   func(context.Context, time.Time, func(runtime.Event) error) error
}

func (f fakeRuntime) List(ctx context.Context) ([]runtime.Container, error) { return f.list(ctx) }
func (f fakeRuntime) Inspect(ctx context.Context, id string) (runtime.Container, error) {
	return f.inspect(ctx, id)
}
func (f fakeRuntime) Watch(ctx context.Context, since time.Time, emit func(runtime.Event) error) error {
	return f.watch(ctx, since, emit)
}

func testApp(rt runtime.Runtime) *App {
	return &App{Runtime: rt, View: &runtime.View{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RequestTimeout: time.Second, RefreshInterval: time.Hour}
}

func await(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("observation did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSnapshotEventsAndShutdown(t *testing.T) {
	events := make(chan runtime.Event)
	exited := make(chan struct{})
	beforeList := time.Now()
	var afterList time.Time
	var exists atomic.Bool
	exists.Store(true)
	a := testApp(fakeRuntime{
		list: func(context.Context) ([]runtime.Container, error) {
			afterList = time.Now()
			return []runtime.Container{{ID: "a", State: "exited"}}, nil
		},
		inspect: func(context.Context, string) (runtime.Container, error) {
			if !exists.Load() {
				return runtime.Container{}, runtime.ErrNotFound
			}
			return runtime.Container{ID: "a", State: "running"}, nil
		},
		watch: func(ctx context.Context, since time.Time, emit func(runtime.Event) error) error {
			defer close(exited)
			if since.Before(beforeList) || since.After(afterList) {
				return errors.New("replay starts outside bootstrap window")
			}
			for {
				select {
				case <-ctx.Done():
					return nil
				case event := <-events:
					if err := emit(event); err != nil {
						return err
					}
				}
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	await(t, func() bool { return len(a.View.Snapshot()) == 1 })
	if a.View.Snapshot()[0].State != "exited" {
		t.Fatal("initial snapshot missing")
	}
	// A delayed destroy hint must not remove a container Docker still has.
	select {
	case events <- runtime.Event{ContainerID: "a", Action: "destroy"}:
	case <-time.After(3 * time.Second):
		t.Fatal("watch not started")
	}
	await(t, func() bool { s := a.View.Snapshot(); return len(s) == 1 && s[0].State == "running" })
	exists.Store(false)
	select {
	case events <- runtime.Event{ContainerID: "a", Action: "die"}:
	case <-time.After(3 * time.Second):
		t.Fatal("watch stalled")
	}
	await(t, func() bool { return len(a.View.Snapshot()) == 0 })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stalled")
	}
	select {
	case <-exited:
	default:
		t.Fatal("watcher not joined")
	}
}

func TestFailuresStopAgent(t *testing.T) {
	boom := errors.New("daemon unavailable")
	for _, phase := range []string{"list", "inspect", "watch", "eof"} {
		t.Run(phase, func(t *testing.T) {
			joined := make(chan struct{})
			rt := fakeRuntime{
				list: func(context.Context) ([]runtime.Container, error) {
					if phase == "list" {
						return nil, boom
					}
					return nil, nil
				},
				inspect: func(context.Context, string) (runtime.Container, error) { return runtime.Container{}, boom },
				watch: func(ctx context.Context, _ time.Time, emit func(runtime.Event) error) error {
					defer close(joined)
					if phase == "watch" {
						return boom
					}
					if phase == "eof" {
						return nil
					}
					if err := emit(runtime.Event{ContainerID: "a"}); err != nil {
						return err
					}
					<-ctx.Done()
					return nil
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err := testApp(rt).Run(ctx)
			if err == nil {
				t.Fatal("failure swallowed")
			}
			if phase != "eof" && !errors.Is(err, boom) {
				t.Fatalf("lost cause: %v", err)
			}
			if phase != "list" {
				select {
				case <-joined:
				default:
					t.Fatal("watcher not joined")
				}
			}
		})
	}
}

func TestPeriodicSnapshotRepairsMissingEvents(t *testing.T) {
	var calls atomic.Int32
	rt := fakeRuntime{
		list: func(context.Context) ([]runtime.Container, error) {
			if calls.Add(1) == 1 {
				return []runtime.Container{{ID: "stale"}}, nil
			}
			return []runtime.Container{{ID: "new"}}, nil
		},
		watch: func(ctx context.Context, _ time.Time, _ func(runtime.Event) error) error { <-ctx.Done(); return nil },
	}
	a := testApp(rt)
	a.RefreshInterval = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	await(t, func() bool { s := a.View.Snapshot(); return len(s) == 1 && s[0].ID == "new" })
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stalled")
	}
}

func TestRequestDeadlineAndCancelledBootstrap(t *testing.T) {
	a := testApp(fakeRuntime{list: func(ctx context.Context) ([]runtime.Container, error) { <-ctx.Done(); return nil, ctx.Err() }})
	a.RequestTimeout = 5 * time.Millisecond
	if err := a.Run(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want request deadline, got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx); err != nil {
		t.Fatalf("cancelled bootstrap: %v", err)
	}
}
