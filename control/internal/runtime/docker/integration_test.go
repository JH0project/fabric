package docker

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/JH0project/JH0project-fabric/control/internal/app"
	"github.com/JH0project/JH0project-fabric/control/internal/runtime"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// This opt-in test mutates only its own unnamed, labeled container. The image
// must already exist locally and provide sleep; no image is pulled by the test.
func TestDockerIntegration(t *testing.T) {
	if os.Getenv("FABRIC_DOCKER_INTEGRATION") != "1" {
		t.Skip("set FABRIC_DOCKER_INTEGRATION=1 to use a real Docker Engine")
	}
	image := os.Getenv("FABRIC_TEST_IMAGE")
	if image == "" {
		image = "alpine:latest"
	}
	rt, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	created, err := rt.client.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config:     &container.Config{Image: image, Entrypoint: []string{"sleep"}, Cmd: []string{"300"}, Labels: map[string]string{"jh0project-fabric.test": "runtime-observation"}},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := created.ID
	t.Cleanup(func() {
		cleanupCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		_, err := rt.client.ContainerRemove(cleanupCtx, id, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
		if err != nil && !errors.Is(err, errdefs.ErrNotFound) {
			t.Error(err)
		}
	})
	view := &runtime.View{}
	a := app.App{Runtime: rt, View: view, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), RequestTimeout: 10 * time.Second, RefreshInterval: time.Hour}
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- a.Run(runCtx) }()
	t.Cleanup(func() {
		stop()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("agent shutdown stalled")
		}
	})
	waitState := func(want string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			state := "absent"
			for _, c := range view.Snapshot() {
				if c.ID == id {
					state = c.State
				}
			}
			if state == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("want %s, got %s", want, state)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitState("created") // proves initial snapshot includes stopped containers
	if _, err := rt.client.ContainerStart(ctx, id, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState("running")
	timeout := 1
	if _, err := rt.client.ContainerStop(ctx, id, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		t.Fatal(err)
	}
	waitState("exited")
	if _, err := rt.client.ContainerRemove(ctx, id, client.ContainerRemoveOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState("absent")
}
