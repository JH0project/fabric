package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/runtime"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestRuntimeTelemetry(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer provider.Shutdown(context.Background())
	exporter := tracetest.NewInMemoryExporter()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer traces.Shutdown(context.Background())
	events := make(chan runtime.Event)
	var exists atomic.Bool
	exists.Store(true)
	a := testApp(fakeRuntime{
		list: func(context.Context) ([]runtime.Container, error) { return nil, nil },
		inspect: func(context.Context, string) (runtime.Container, error) {
			if !exists.Load() {
				return runtime.Container{}, runtime.ErrNotFound
			}
			return runtime.Container{ID: "a", State: "running"}, nil
		},
		watch: func(ctx context.Context, _ time.Time, emit func(runtime.Event) error) error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case event := <-events:
					if err := emit(event); err != nil {
						return err
					}
				}
			}
		},
	})
	a.Meter = provider.Meter("test")
	a.Tracer = traces.Tracer("test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	send := func(action string) {
		t.Helper()
		select {
		case events <- runtime.Event{ContainerID: "a", Action: action}:
		case <-time.After(3 * time.Second):
			t.Fatal("watch stalled")
		}
	}
	collect := func() metricdata.ResourceMetrics {
		t.Helper()
		var data metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &data); err != nil {
			t.Fatal(err)
		}
		return data
	}
	points := func(data metricdata.ResourceMetrics, name string) map[string]int64 {
		t.Helper()
		result := map[string]int64{}
		for _, scope := range data.ScopeMetrics {
			for _, m := range scope.Metrics {
				if m.Name != name {
					continue
				}
				var dataPoints []metricdata.DataPoint[int64]
				switch d := m.Data.(type) {
				case metricdata.Sum[int64]:
					dataPoints = d.DataPoints
				case metricdata.Gauge[int64]:
					dataPoints = d.DataPoints
				}
				for _, p := range dataPoints {
					attrs := p.Attributes.ToSlice()
					if len(attrs) != 1 {
						t.Fatalf("unexpected metric dimensions: %v", attrs)
					}
					result[attrs[0].Value.AsString()] = p.Value
				}
			}
		}
		return result
	}
	send("health_status: unbounded-value")
	await(t, func() bool {
		d := collect()
		return points(d, "fabric.runtime.containers")["running"] == 1 && points(d, "fabric.runtime.inspects")["success"] == 1
	})
	exists.Store(false)
	send("unbounded-unknown-action")
	await(t, func() bool {
		d := collect()
		return points(d, "fabric.runtime.containers")["running"] == 0 && points(d, "fabric.runtime.inspects")["success"] == 2
	})
	d := collect()
	actions := points(d, "fabric.runtime.events")
	if len(actions) != 2 || actions["health_status"] != 1 || actions["other"] != 1 {
		t.Fatalf("action cardinality: %v", actions)
	}
	gauges := points(d, "fabric.runtime.containers")
	if len(gauges) != len(containerStates) {
		t.Fatalf("missing zero states: %v", gauges)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stalled")
	}
	spans := exporter.GetSpans()
	if len(spans) != 3 || spans[0].Name != "docker.snapshot" {
		t.Fatalf("operation spans: %+v", spans)
	}
	for _, s := range spans {
		if s.Status.Code == codes.Error {
			t.Fatal("expected deletion recorded as error")
		}
	}
}

func TestFailureRecordedOnOperationSpan(t *testing.T) {
	exporter := tracetest.NewInMemoryExporter()
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	defer traces.Shutdown(context.Background())
	boom := errors.New("snapshot failed")
	a := testApp(fakeRuntime{list: func(context.Context) ([]runtime.Container, error) { return nil, boom }})
	a.Tracer = traces.Tracer("test")
	if err := a.Run(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("lost cause: %v", err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 1 || spans[0].Status.Code != codes.Error || len(spans[0].Events) != 1 {
		t.Fatalf("failure span: %+v", spans)
	}
}
