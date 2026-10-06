package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/H01001000/jh0project-fabric/control/internal/app"
	"github.com/H01001000/jh0project-fabric/control/internal/runtime"
	"github.com/H01001000/jh0project-fabric/control/internal/runtime/docker"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, logger, os.Args[1:])
	stop()
	if err != nil {
		logger.Error("agentd stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, args []string) (err error) {
	flags := flag.NewFlagSet("agentd", flag.ContinueOnError)
	timeout := flags.Duration("request-timeout", 10*time.Second, "timeout for Docker list and inspect requests")
	refresh := flags.Duration("refresh-interval", 30*time.Second, "interval for a full Docker observation")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("agentd accepts no positional arguments")
	}
	if *timeout <= 0 || *refresh <= 0 {
		return errors.New("observation intervals must be positive")
	}
	rt, err := docker.New()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rt.Close()) }()
	a := app.App{
		Runtime: rt, View: &runtime.View{}, Logger: logger,
		RequestTimeout: *timeout, RefreshInterval: *refresh,
	}
	logger.Info("agentd starting")
	if err := a.Run(ctx); err != nil {
		return fmt.Errorf("agentd: %w", err)
	}
	logger.Info("agentd stopped")
	return nil
}
