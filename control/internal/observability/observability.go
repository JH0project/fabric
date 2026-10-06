// Package observability bootstraps native OTel SDKs; feature packages use OTel APIs.
package observability

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

type Config struct {
	ServiceName    string
	ServiceVersion string
	Level          slog.Level
	Format         string
	Output         io.Writer
}

type Telemetry struct {
	Logger  *slog.Logger
	Traces  *sdktrace.TracerProvider
	Metrics *sdkmetric.MeterProvider
	Logs    *sdklog.LoggerProvider
	console *slog.Logger
}

func New(ctx context.Context, cfg Config) (_ *Telemetry, err error) {
	if cfg.ServiceName == "" || cfg.ServiceVersion == "" {
		return nil, errors.New("service name and version are required")
	}
	if cfg.Output == nil {
		cfg.Output = os.Stderr
	}
	if cfg.Format != "text" && cfg.Format != "json" {
		return nil, errors.New("log format must be text or json")
	}
	res, err := newResource(ctx, cfg)
	if err != nil {
		return nil, err
	}
	options := &slog.HandlerOptions{Level: cfg.Level}
	var console slog.Handler = slog.NewTextHandler(cfg.Output, options)
	if cfg.Format == "json" {
		console = slog.NewJSONHandler(cfg.Output, options)
	}
	attrs := make([]slog.Attr, 0, len(res.Attributes()))
	for _, a := range res.Attributes() {
		attrs = append(attrs, slog.Any(string(a.Key), a.Value.AsInterface()))
	}
	console = console.WithAttrs(attrs)
	t := &Telemetry{console: slog.New(console)}
	// Tear down partially constructed exporters if configuration fails.
	defer func() {
		if err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = t.Shutdown(cleanupCtx)
		}
	}()
	traceOptions := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		traceOptions = append(traceOptions, sdktrace.WithSampler(sdktrace.NeverSample()))
	}
	metricOptions := []sdkmetric.Option{sdkmetric.WithResource(res)}
	logOptions := []sdklog.LoggerProviderOption{sdklog.WithResource(res)}
	for _, signal := range []string{"TRACES", "METRICS", "LOGS"} {
		enabled, protocol, e := signalConfig(signal)
		if e != nil {
			return nil, e
		}
		if !enabled {
			continue
		}
		switch signal {
		case "TRACES":
			var exporter sdktrace.SpanExporter
			if protocol == "grpc" {
				exporter, e = otlptracegrpc.New(ctx)
			} else {
				exporter, e = otlptracehttp.New(ctx)
			}
			if e != nil {
				return nil, fmt.Errorf("configure traces: %w", e)
			}
			traceOptions = append(traceOptions, sdktrace.WithBatcher(exporter))
			// Own each exporter immediately, so later failures can clean it up.
			t.Traces = sdktrace.NewTracerProvider(traceOptions...)
		case "METRICS":
			var exporter sdkmetric.Exporter
			if protocol == "grpc" {
				exporter, e = otlpmetricgrpc.New(ctx)
			} else {
				exporter, e = otlpmetrichttp.New(ctx)
			}
			if e != nil {
				return nil, fmt.Errorf("configure metrics: %w", e)
			}
			metricOptions = append(metricOptions, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)))
			t.Metrics = sdkmetric.NewMeterProvider(metricOptions...)
		case "LOGS":
			var exporter sdklog.Exporter
			if protocol == "grpc" {
				exporter, e = otlploggrpc.New(ctx)
			} else {
				exporter, e = otlploghttp.New(ctx)
			}
			if e != nil {
				return nil, fmt.Errorf("configure logs: %w", e)
			}
			logOptions = append(logOptions, sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)))
			t.Logs = sdklog.NewLoggerProvider(logOptions...)
		}
	}
	if t.Traces == nil {
		t.Traces = sdktrace.NewTracerProvider(traceOptions...)
	}
	if t.Metrics == nil {
		t.Metrics = sdkmetric.NewMeterProvider(metricOptions...)
	}
	if t.Logs == nil {
		t.Logs = sdklog.NewLoggerProvider(logOptions...)
	}
	t.Logger = slog.New(&handler{console: console, export: otelslog.NewHandler("fabric/agentd", otelslog.WithLoggerProvider(t.Logs)), level: cfg.Level})
	return t, nil
}

// Install is called once by process bootstrap, not by feature packages or tests.
func (t *Telemetry) Install() {
	otel.SetTracerProvider(t.Traces)
	otel.SetMeterProvider(t.Metrics)
	logglobal.SetLoggerProvider(t.Logs)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	// Use console only: sending exporter failures back to OTLP recurses.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { t.console.Warn("telemetry export failed", "error", err) }))
}

func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	if t.Traces != nil {
		errs = append(errs, t.Traces.Shutdown(ctx))
	}
	if t.Metrics != nil {
		errs = append(errs, t.Metrics.Shutdown(ctx))
	}
	if t.Logs != nil {
		errs = append(errs, t.Logs.Shutdown(ctx))
	}
	return errors.Join(errs...)
}

func newResource(ctx context.Context, cfg Config) (*resource.Resource, error) {
	host, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	value := func(key, fallback string) string {
		if v := os.Getenv(key); v != "" {
			return v
		}
		return fallback
	}
	return resource.New(ctx, resource.WithAttributes(
		attribute.String("service.name", cfg.ServiceName), attribute.String("service.version", cfg.ServiceVersion),
		attribute.String("service.instance.id", hex.EncodeToString(id)),
		attribute.String("deployment.environment.name", value("FABRIC_ENVIRONMENT", "development")),
		attribute.String("jh0project.node.id", value("FABRIC_NODE_ID", host)),
		attribute.String("jh0project.node.name", value("FABRIC_NODE_NAME", host)),
		attribute.String("jh0project.node.region", value("FABRIC_NODE_REGION", "unknown")),
		attribute.String("jh0project.node.zone", value("FABRIC_NODE_ZONE", "unknown")),
	), resource.WithFromEnv())
}

// Export is opt-in in local development. Standard per-signal exporter and
// endpoint variables take precedence; SDKs handle TLS, headers and timeouts.
func signalConfig(signal string) (bool, string, error) {
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return false, "", nil
	}
	exporter := os.Getenv("OTEL_" + signal + "_EXPORTER")
	if exporter == "none" {
		return false, "", nil
	}
	if exporter != "" && exporter != "otlp" {
		return false, "", fmt.Errorf("OTEL_%s_EXPORTER must be otlp or none", signal)
	}
	if exporter == "" && os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_"+signal+"_ENDPOINT") == "" {
		return false, "", nil
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_" + signal + "_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol == "" {
		protocol = "http/protobuf"
	}
	if protocol != "grpc" && protocol != "http/protobuf" {
		return false, "", fmt.Errorf("unsupported OTLP %s protocol %q", signal, protocol)
	}
	return true, protocol, nil
}
