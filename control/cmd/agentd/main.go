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
	"strings"
	"syscall"
	"time"

	"github.com/JH0project/fabric/control/internal/app"
	"github.com/JH0project/fabric/control/internal/configuration"
	"github.com/JH0project/fabric/control/internal/discovery"
	"github.com/JH0project/fabric/control/internal/observability"
	"github.com/JH0project/fabric/control/internal/rpc"
	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/JH0project/fabric/control/internal/runtime/docker"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
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
	rpcEnabled := flags.Bool("rpc-enabled", os.Getenv("FABRIC_RPC_ENABLED") == "true", "enable authenticated configuration RPC")
	rpcListen := flags.String("rpc-listen", envOr("FABRIC_RPC_LISTEN", "127.0.0.1:9781"), "configuration RPC listen address")
	roots := flags.String("configuration-roots", os.Getenv("FABRIC_CONFIGURATION_ROOTS"), "comma-separated writable configuration roots")
	dnsEnabled := flags.Bool("dns-enabled", os.Getenv("FABRIC_DNS_ENABLED") == "true", "enable embedded local service DNS")
	dnsListen := flags.String("dns-listen", envOr("FABRIC_DNS_LISTEN", "127.0.0.1:8600"), "DNS UDP and TCP listen address")
	dnsZone := flags.String("dns-zone", envOr("FABRIC_DNS_ZONE", "fabric.internal"), "authoritative service DNS zone")
	hostname, _ := os.Hostname()
	dnsNode := flags.String("dns-node", envOr("FABRIC_NODE_ID", strings.ToLower(hostname)), "node DNS label; local is reserved")
	dnsTTL := flags.Duration("dns-ttl", 5*time.Second, "DNS record and negative-cache TTL (whole seconds, 1s to 1m)")
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
	var dnsServer *discovery.DNS
	if *dnsEnabled {
		if *dnsTTL < time.Second || *dnsTTL > time.Minute || *dnsTTL%time.Second != 0 {
			return errors.New("DNS TTL must be a whole number of seconds between 1s and 1m")
		}
		cache, cacheErr := discovery.NewCache(*dnsZone, *dnsNode)
		if cacheErr != nil {
			return cacheErr
		}
		dnsServer = &discovery.DNS{Cache: cache, TTL: uint32(*dnsTTL / time.Second), Address: *dnsListen}
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
	rt.IncludeDiscovery = *dnsEnabled
	a := app.App{
		Runtime: rt, View: &runtime.View{}, Logger: logger,
		RequestTimeout: *timeout, RefreshInterval: *refresh,
	}
	var additional []func(context.Context) error
	if dnsServer != nil {
		dnsServer.OnListening = func(address string) {
			logger.InfoContext(ctx, "embedded DNS listening", "server.address", address, "fabric.dns.zone", dnsServer.Cache.Zone)
		}
		previousInvalid := ""
		a.OnSnapshot = func(containers []runtime.Container) {
			endpoints, invalid := discovery.FromContainers(dnsServer.Cache.Node, containers)
			if key := strings.Join(invalid, ","); key != previousInvalid {
				if len(invalid) != 0 {
					logger.Warn("invalid discovery registrations omitted", "fabric.discovery.invalid", len(invalid))
				}
				previousInvalid = key
			}
			if err := dnsServer.Cache.Replace(endpoints); err != nil {
				logger.Warn("DNS snapshot retained", "error", err)
			}
		}
		additional = append(additional, dnsServer.Run)
		logger.InfoContext(ctx, "embedded DNS enabled", "server.address", *dnsListen, "fabric.dns.zone", dnsServer.Cache.Zone, "fabric.dns.scope", "local")
	}
	var server *rpc.Server
	if *rpcEnabled {
		var applier rpc.Applier
		// Compose generation is stateless and can run without enabling file writes.
		if strings.TrimSpace(*roots) != "" {
			paths := strings.Split(*roots, ",")
			for i := range paths {
				paths[i] = strings.TrimSpace(paths[i])
			}
			writer, writerErr := configuration.NewWriter(paths)
			if writerErr != nil {
				return writerErr
			}
			defer writer.Close()
			service, serviceErr := configuration.NewService(writer, logger)
			if serviceErr != nil {
				return serviceErr
			}
			applier = service
		}
		server, err = rpc.New(*rpcListen, os.Getenv("FABRIC_RPC_TOKEN"), applier)
		if err != nil {
			return err
		}
	}
	logger.InfoContext(ctx, "agentd starting")
	if err := runServices(ctx, a.Run, server, additional...); err != nil {
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

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func runServices(ctx context.Context, observe func(context.Context) error, server *rpc.Server, additional ...func(context.Context) error) error {
	services := append([]func(context.Context) error{observe}, additional...)
	if server != nil {
		services = append(services, server.Run)
	}
	if len(services) == 1 {
		return observe(ctx)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(services))
	for _, service := range services {
		go func() { done <- service(ctx) }()
	}
	first := <-done
	cancel()
	for i := 1; i < len(services); i++ {
		first = errors.Join(first, <-done)
	}
	return first
}
