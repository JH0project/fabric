package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

func TestArgumentsAndCancelledBootstrap(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, args := range [][]string{{"-request-timeout=0"}, {"-refresh-interval=-1s"}, {"unexpected"}, {"-log-level=bogus"}, {"-log-format=bogus"}} {
		if err := run(context.Background(), logger, args); err == nil {
			t.Fatalf("accepted %v", args)
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
