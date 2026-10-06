package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	logpb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	metricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
)

func cleanEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"OTEL_SDK_DISABLED", "OTEL_SERVICE_NAME", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_TRACES_SAMPLER", "OTEL_TRACES_SAMPLER_ARG"} {
		t.Setenv(key, "")
	}
	for _, signal := range []string{"TRACES", "METRICS", "LOGS"} {
		for _, suffix := range []string{"ENDPOINT", "PROTOCOL", "HEADERS", "TIMEOUT"} {
			t.Setenv("OTEL_EXPORTER_OTLP_"+signal+"_"+suffix, "")
		}
		t.Setenv("OTEL_"+signal+"_EXPORTER", "")
	}
}

func TestSignalConfiguration(t *testing.T) {
	cleanEnv(t)
	for _, signal := range []string{"TRACES", "METRICS", "LOGS"} {
		if on, protocol, err := signalConfig(signal); !on || protocol != "http/protobuf" || err != nil {
			t.Fatalf("default %s: %v %s %v", signal, on, protocol, err)
		}
	}
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4318")
	if on, p, err := signalConfig("TRACES"); !on || p != "http/protobuf" || err != nil {
		t.Fatalf("HTTP: %v %s %v", on, p, err)
	}
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "grpc")
	if _, p, _ := signalConfig("TRACES"); p != "grpc" {
		t.Fatal("signal protocol did not override")
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	if on, _, _ := signalConfig("TRACES"); on {
		t.Fatal("none ignored")
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "bogus")
	if _, _, err := signalConfig("TRACES"); err == nil {
		t.Fatal("invalid exporter accepted")
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL", "bogus")
	if _, _, err := signalConfig("TRACES"); err == nil {
		t.Fatal("invalid protocol accepted")
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if on, _, err := signalConfig("TRACES"); on || err != nil {
		t.Fatal("SDK disabled ignored")
	}
}

func TestOTLPAllSignalsAndConsoleCorrelation(t *testing.T) {
	cleanEnv(t)
	var mu sync.Mutex
	var traces tracepb.ExportTraceServiceRequest
	var metrics metricpb.ExportMetricsServiceRequest
	var logs logpb.ExportLogsServiceRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		var message proto.Message
		switch r.URL.Path {
		case "/v1/traces":
			message = &traces
		case "/v1/metrics":
			message = &metrics
		case "/v1/logs":
			message = &logs
		default:
			t.Errorf("wrong path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if err := proto.Unmarshal(body, message); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	t.Setenv("OTEL_SERVICE_NAME", "test-agent")
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "jh0project.node.id=stable-node,jh0project.node.region=test-region")
	var console bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	obs, err := New(ctx, Config{ServiceName: "agentd", ServiceVersion: "test-version", Format: "json", Output: &console})
	if err != nil {
		t.Fatal(err)
	}
	spanCtx, span := obs.Traces.Tracer("test").Start(ctx, "docker.snapshot")
	obs.Logger.DebugContext(spanCtx, "hidden")
	obs.Logger.With("operation", "snapshot").InfoContext(spanCtx, "snapshot loaded")
	span.End()
	counter, err := obs.Metrics.Meter("test").Int64Counter("fabric.runtime.snapshots")
	if err != nil {
		t.Fatal(err)
	}
	counter.Add(ctx, 1)
	cancel() // flushing must work with a fresh context after application cancellation
	flushCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := obs.Shutdown(flushCtx); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(console.Bytes()), &record); err != nil {
		t.Fatal(err)
	}
	if record["trace_id"] != span.SpanContext().TraceID().String() || record["span_id"] != span.SpanContext().SpanID().String() || record["service.name"] != "test-agent" {
		t.Fatalf("console correlation: %v", record)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(traces.ResourceSpans) != 1 || len(metrics.ResourceMetrics) != 1 || len(logs.ResourceLogs) != 1 {
		t.Fatalf("missing signals: traces=%d metrics=%d logs=%d", len(traces.ResourceSpans), len(metrics.ResourceMetrics), len(logs.ResourceLogs))
	}
	checkResource := func(res *resourcepb.Resource) {
		t.Helper()
		attrs := map[string]string{}
		for _, kv := range res.Attributes {
			attrs[kv.Key] = kv.Value.GetStringValue()
		}
		for _, key := range []string{"service.name", "service.version", "service.instance.id", "jh0project.node.id", "jh0project.node.name", "jh0project.node.region", "jh0project.node.zone", "deployment.environment.name"} {
			if attrs[key] == "" {
				t.Errorf("missing resource %s", key)
			}
		}
		if attrs["service.name"] != "test-agent" || attrs["jh0project.node.id"] != "stable-node" {
			t.Errorf("override lost: %v", attrs)
		}
	}
	checkResource(traces.ResourceSpans[0].Resource)
	checkResource(metrics.ResourceMetrics[0].Resource)
	checkResource(logs.ResourceLogs[0].Resource)
	logRecord := logs.ResourceLogs[0].ScopeLogs[0].LogRecords[0]
	traceID := span.SpanContext().TraceID()
	if !bytes.Equal(logRecord.TraceId, traceID[:]) {
		t.Fatal("OTLP log lost trace context")
	}
}

func TestLocalModeAndUnavailableCollector(t *testing.T) {
	cleanEnv(t)
	for _, signal := range []string{"TRACES", "METRICS", "LOGS"} {
		t.Setenv("OTEL_"+signal+"_EXPORTER", "none")
	}
	obs, err := New(context.Background(), Config{ServiceName: "agentd", ServiceVersion: "dev", Format: "text", Output: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err := obs.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", server.URL+"/v1/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_TIMEOUT", "100")
	obs, err = New(context.Background(), Config{ServiceName: "agentd", ServiceVersion: "dev", Format: "text", Output: io.Discard})
	if err != nil {
		t.Fatalf("collector absence prevented bootstrap: %v", err)
	}
	_, span := obs.Traces.Tracer("test").Start(context.Background(), "test")
	span.End()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := obs.Shutdown(ctx); err == nil {
		t.Fatal("expected failed flush to be reported")
	}
}

func TestResourceInstanceUniqueness(t *testing.T) {
	cleanEnv(t)
	a, err := newResource(context.Background(), Config{ServiceName: "agentd", ServiceVersion: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := newResource(context.Background(), Config{ServiceName: "agentd", ServiceVersion: "dev"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(a.String(), "service.instance.id=") && a.String() == b.String() {
		t.Fatal("instance IDs reused")
	}
}
