package configuration

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"io"
	"io/fs"
	"log/slog"
	"testing"
)

type failingWriter struct{ err error }

func (w failingWriter) Write(context.Context, File) (string, error) { return "", w.err }
func TestErrorCodesAndTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer metrics.Shutdown(context.Background())
	exporter := tracetest.NewInMemoryExporter()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer traces.Shutdown(context.Background())
	oldMetrics, oldTraces := otel.GetMeterProvider(), otel.GetTracerProvider()
	otel.SetMeterProvider(metrics)
	otel.SetTracerProvider(traces)
	defer func() { otel.SetMeterProvider(oldMetrics); otel.SetTracerProvider(oldTraces) }()
	for _, tc := range []struct {
		err  error
		code string
	}{
		{fs.ErrPermission, "permission_denied"}, {context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"}, {ErrInvalid, "invalid_file"},
		{ErrConflict, "path_conflict"}, {errors.New("secret input"), "write_failed"},
	} {
		service, err := NewService(failingWriter{tc.err}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		result := service.Apply(context.Background(), Request{ServiceID: "app", Files: []File{{Path: "file", Content: ptr("secret input")}}})
		if result.Applied || result.Files[0].Code != tc.code {
			t.Fatal(result)
		}
	}
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == "fabric.configuration.apply" {
				found = true
				for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
					if p.Attributes.Len() != 1 {
						t.Fatal("unbounded metric dimensions")
					}
					if _, ok := p.Attributes.Value(attribute.Key("result")); !ok {
						t.Fatal("missing outcome")
					}
				}
			}
		}
	}
	if !found {
		t.Fatal("no application metrics")
	}
	if len(exporter.GetSpans()) != 6 {
		t.Fatal("missing operation spans")
	}
}
