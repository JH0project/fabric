package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	goruntime "runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/JH0project/fabric/control/internal/app"
	"github.com/JH0project/fabric/control/internal/configuration"
	"github.com/JH0project/fabric/control/internal/discovery"
	"github.com/JH0project/fabric/control/internal/inventory"
	"github.com/JH0project/fabric/control/internal/observability"
	"github.com/JH0project/fabric/control/internal/rpc"
	"github.com/JH0project/fabric/control/internal/runtime"
	"github.com/JH0project/fabric/control/internal/runtime/docker"
	"github.com/google/uuid"
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
	dnsNode := flags.String("dns-node", envOr("FABRIC_NODE_NAME", envOr("FABRIC_NODE_ID", strings.ToLower(hostname))), "node DNS label; local is reserved")
	dnsTTL := flags.Duration("dns-ttl", 5*time.Second, "DNS record and negative-cache TTL (whole seconds, 1s to 1m)")
	inventoryEnabled := flags.Bool("inventory-enabled", os.Getenv("FABRIC_INVENTORY_ENABLED") == "true", "publish observed RuntimeInstances to Core NATS")
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
	var nodeUUID uuid.UUID
	if *inventoryEnabled {
		explicitListen := false
		flags.Visit(func(f *flag.Flag) {
			if f.Name == "dns-listen" {
				explicitListen = true
			}
		})
		if !explicitListen && os.Getenv("FABRIC_DNS_LISTEN") == "" {
			*dnsListen = "[::1]:8600"
		}
		var parseErr error
		nodeUUID, parseErr = uuid.Parse(os.Getenv("FABRIC_NODE_UUID"))
		if parseErr != nil || nodeUUID == uuid.Nil || !inventory.ValidName(*dnsNode) || os.Getenv("FABRIC_INVENTORY_STATE_DIR") == "" {
			return errors.New("inventory requires FABRIC_NODE_UUID, a valid node name and persistent FABRIC_INVENTORY_STATE_DIR")
		}
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
	rt.IncludeInventory = *inventoryEnabled
	observer := app.Observer{
		Runtime: rt, View: &runtime.View{}, Logger: logger,
		RequestTimeout: *timeout, RefreshInterval: *refresh,
	}
	components := []app.Component{&observer}
	if dnsServer != nil {
		dnsServer.OnListening = func(address string) {
			logger.InfoContext(ctx, "embedded DNS listening", "server.address", address, "fabric.dns.zone", dnsServer.Cache.Zone)
		}
		previousInvalid := ""
		observer.OnSnapshot = func(containers []runtime.Container) {
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
		components = append(components, dnsServer)
		logger.InfoContext(ctx, "embedded DNS enabled", "server.address", *dnsListen, "fabric.dns.zone", dnsServer.Cache.Zone, "fabric.dns.scope", "local")
	}
	if *inventoryEnabled {
		epoch, epochErr := inventory.AcquireEpoch(os.Getenv("FABRIC_INVENTORY_STATE_DIR"), nodeUUID.String())
		if epochErr != nil {
			return epochErr
		}
		defer epoch.Close()
		bus, busErr := inventory.BusFromEnv()
		if busErr != nil {
			return busErr
		}
		defer bus.Close()
		var cache *discovery.Cache
		if dnsServer != nil {
			cache = dnsServer.Cache
			dnsServer.IPv6Only = true
		}
		worker := inventory.NewAgent(nodeUUID, *dnsNode, epoch.Number, bus, logger, cache)
		observer.OnSnapshot = nil // The inventory projection owns the combined DNS index.
		observer.OnObservation = worker.Observe
		components = append(components, worker)
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
		components = append(components, server)
	}
	logger.InfoContext(ctx, "agentd starting",
		"service.name", "agentd", "service.version", buildVersion(),
		"jh0project.node.uuid", envOr("FABRIC_NODE_UUID", "unknown"),
		"jh0project.node.id", envOr("FABRIC_NODE_SEQUENCE_ID", envOr("FABRIC_NODE_ID", hostname)),
		"jh0project.node.name", envOr("FABRIC_NODE_NAME", *dnsNode),
		"jh0project.node.region", envOr("FABRIC_NODE_REGION", "unknown"),
		"jh0project.node.zone", envOr("FABRIC_NODE_ZONE", "unknown"),
		"os.type", goruntime.GOOS, "host.arch", goruntime.GOARCH,
		"process.runtime.version", goruntime.Version(),
		"fabric.dns.enabled", *dnsEnabled, "fabric.rpc.enabled", *rpcEnabled,
		"fabric.inventory.enabled", *inventoryEnabled,
		"fabric.observation.refresh_interval", refresh.String())
	if err := app.New(components...).Run(ctx); err != nil {
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
