package main

import (
	"context"
	"errors"
	"github.com/JH0project/fabric/control/internal/configuration"
	"github.com/JH0project/fabric/control/internal/rpc"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"
)

func TestArgumentsAndCancelledBootstrap(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{"-request-timeout=0"}, {"-refresh-interval=-1s"}, {"unexpected"}, {"-log-level=bogus"}, {"-log-format=bogus"}} {
		if err := run(context.Background(), logger, args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, args := range [][]string{{"-dns-enabled", "-dns-ttl=0"}, {"-dns-enabled", "-dns-ttl=500ms"}, {"-dns-enabled", "-dns-ttl=61s"}, {"-dns-enabled", "-dns-zone=bad..zone"}, {"-dns-enabled", "-dns-node=bad_node"}, {"-dns-enabled", "-dns-node=local"}} {
		if err := run(context.Background(), logger, args); err == nil {
			t.Fatalf("accepted DNS configuration %v", args)
		}
	}
	if err := run(context.Background(), logger, []string{"-h"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := run(ctx, logger, nil); err != nil {
		t.Fatal(err)
	}
}

type testApplier struct{}

func (testApplier) Apply(context.Context, configuration.Request) configuration.Result {
	return configuration.Result{Applied: true}
}
func TestCombinedServicesJoinOnFailure(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server, err := rpc.New(listener.Addr().String(), "01234567890123456789012345678901", testApplier{})
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runServices(context.Background(), func(ctx context.Context) error { <-ctx.Done(); close(stopped); return nil }, server)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("RPC listen failure ignored")
		}
	case <-time.After(time.Second):
		t.Fatal("services did not stop")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("observer was not joined")
	}
	boom := errors.New("observer failed")
	server.Address = "127.0.0.1:0"
	if err := runServices(context.Background(), func(context.Context) error { return boom }, server); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
