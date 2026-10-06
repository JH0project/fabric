package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/H01001000/jh0project-fabric/control/internal/app"
	"github.com/H01001000/jh0project-fabric/control/internal/observability"
	"github.com/H01001000/jh0project-fabric/control/internal/runtime"
	"github.com/H01001000/jh0project-fabric/control/internal/runtime/docker"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, logger, os.Args[1:])
	stop()
	if err != nil {
		var reported reportedError
		if !errors.As(err, &reported) {
			logger.Error("agentd stopped", "error", err)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, logger *slog.Logger, args []string) (err error) {
	flags := flag.NewFlagSet("agentd", flag.ContinueOnError)
	timeout := flags.Duration("request-timeout", 10*time.Second, "timeout for Docker list and inspect requests")
	refresh := flags.Duration("refresh-interval", 30*time.Second, "interval for a full Docker observation")
	logLevel := flags.String("log-level", "info", "debug, info, warn, or error")
	logFormat := flags.String("log-format", "text", "text or json")
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
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return err
	}
	if *logFormat != "text" && *logFormat != "json" {
		return errors.New("log format must be text or json")
	}
	if ctx.Err() != nil {
		return nil
	}
	obs, err := observability.New(ctx, observability.Config{ServiceName: "agentd", ServiceVersion: buildVersion(), Level: level, Format: *logFormat})
	if err != nil {
		return err
	}
	obs.Install()
	logger = obs.Logger
	defer func() {
		// Signals cancel the run context; flushing requires a fresh bounded one.
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if flushErr := obs.Shutdown(flushCtx); flushErr != nil {
			logger.Warn("telemetry shutdown incomplete", "error", flushErr)
		}
	}()
	// Record an operational failure once, before exporters are shut down.
	defer func() {
		if err != nil {
			logger.ErrorContext(ctx, "agentd stopped", "error", err)
			err = reportedError{err}
		}
	}()
	rt, err := docker.New()
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, rt.Close()) }()
	a := app.App{
		Runtime: rt, View: &runtime.View{}, Logger: logger,
		RequestTimeout: *timeout, RefreshInterval: *refresh,
	}
	logger.InfoContext(ctx, "agentd starting")
	if err := a.Run(ctx); err != nil {
		return fmt.Errorf("agentd: %w", err)
	}
	logger.InfoContext(ctx, "agentd stopped")
	return nil
}

var version = "dev" // override with -ldflags '-X main.version=<release>'
type reportedError struct{ error }

func (e reportedError) Unwrap() error { return e.error }

func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return version
}
