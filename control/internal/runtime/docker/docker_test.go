package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/moby/moby/client"
)

func testRuntime(t *testing.T, handler http.HandlerFunc) *Runtime {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.51"))
	if err != nil {
		t.Fatal(err)
	}
	r := &Runtime{client: c}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r
}

func TestListAndInspectMapping(t *testing.T) {
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1.51/containers/json":
			if req.URL.Query().Get("all") != "1" {
				t.Error("stopped containers excluded")
			}
			fmt.Fprint(w, `[{"Id":"a","Names":["/web"],"Image":"alpine:latest","State":"exited","Labels":{"owner":"fabric"}}]`)
		case "/v1.51/containers/a/json":
			fmt.Fprint(w, `{"Id":"a","Name":"/web","Config":{"Image":"alpine:latest","Labels":{"owner":"fabric"}},"State":{"Status":"running"}}`)
		case "/v1.51/containers/bad/json":
			fmt.Fprint(w, `{"Id":"bad"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"No such container"}`)
		}
	})
	ctx := context.Background()
	containers, err := r.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 1 || containers[0].Name != "web" || containers[0].State != "exited" || containers[0].Labels["owner"] != "fabric" {
		t.Fatalf("bad list mapping: %+v", containers)
	}
	c, err := r.Inspect(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != "a" || c.Name != "web" || c.Image != "alpine:latest" || c.State != "running" {
		t.Fatalf("bad inspect mapping: %+v", c)
	}
	if _, err := r.Inspect(ctx, "missing"); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("not-found mapping: %v", err)
	}
	if _, err := r.Inspect(ctx, "bad"); err == nil {
		t.Fatal("incomplete inspect accepted")
	}
}

func TestWatchMappingAndEOF(t *testing.T) {
	since := time.Now().Add(-time.Minute)
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1.51/events" || req.URL.Query().Get("since") == "" || !strings.Contains(req.URL.Query().Get("filters"), "container") {
			t.Errorf("bad event request: %s", req.URL)
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"Type":"container","Action":"start","Actor":{"ID":"a"}}`)
		// Wait for cancellation rather than racing EOF against the message.
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var got runtime.Event
	boom := errors.New("callback stopped")
	err := r.Watch(ctx, since, func(e runtime.Event) error { got = e; return boom })
	if !errors.Is(err, boom) || got.ContainerID != "a" || got.Action != "start" {
		t.Fatalf("event/callback: %+v %v", got, err)
	}

	eof := testRuntime(t, func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("Content-Type", "application/x-ndjson") })
	if err := eof.Watch(ctx, since, func(runtime.Event) error { return nil }); !errors.Is(err, io.EOF) {
		t.Fatalf("want EOF: %v", err)
	}
}

func TestWatchCancellationAndHTTPFailure(t *testing.T) {
	connected := make(chan struct{})
	closed := make(chan struct{})
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(connected)
		<-req.Context().Done()
		close(closed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.Watch(ctx, time.Now(), func(runtime.Event) error { return nil }) }()
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("stream not connected")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watch did not stop")
	}
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("stream still open")
	}
	failed := testRuntime(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"message":"unavailable"}`)
	})
	if err := failed.Watch(context.Background(), time.Now(), func(runtime.Event) error { return nil }); err == nil {
		t.Fatal("HTTP failure swallowed")
	}
}

func TestDiscoverySnapshotInspectsHealthAndNetworks(t *testing.T) {
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch req.URL.Path {
		case "/v1.51/containers/json":
			fmt.Fprint(w, `[{"Id":"a","State":"running","Labels":{"fabric.discovery.service":"api"}},{"Id":"gone","State":"running","Labels":{"fabric.discovery.service":"api"}},{"Id":"unlabelled","State":"running"}]`)
		case "/v1.51/containers/a/json":
			fmt.Fprint(w, `{"Id":"a","Name":"/api","Config":{"Image":"alpine","Labels":{"fabric.discovery.service":"api"}},"State":{"Status":"running","Health":{"Status":"unhealthy"}},"NetworkSettings":{"Networks":{"mesh":{"IPAddress":"10.42.0.2","GlobalIPv6Address":"fd00::2"}}}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"No such container"}`)
		}
	})
	r.IncludeDiscovery = true
	containers, err := r.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(containers) != 2 || containers[0].Health != "unhealthy" || len(containers[0].Networks["mesh"]) != 2 {
		t.Fatalf("discovery mapping: %+v", containers)
	}
	if containers[0].Networks["mesh"][0] != "10.42.0.2" || containers[0].Networks["mesh"][1] != "fd00::2" {
		t.Fatal("IP mapping")
	}
}

func TestNetworkEventsInvalidateContainer(t *testing.T) {
	r := testRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if !strings.Contains(req.URL.Query().Get("filters"), "network") {
			t.Error("missing network filter")
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"Type":"network","Action":"connect","Actor":{"ID":"network-id","Attributes":{"container":"container-id"}}}`)
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	})
	r.IncludeDiscovery = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stop := errors.New("stop")
	err := r.Watch(ctx, time.Now(), func(e runtime.Event) error {
		if e.ContainerID != "container-id" || e.Action != "connect" {
			t.Errorf("bad network event: %+v", e)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
}
