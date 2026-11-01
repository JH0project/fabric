package inventory

import (
	"context"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"time"
)

type operation struct {
	count    metric.Int64Counter
	duration metric.Float64Histogram
	tracer   trace.Tracer
	name     string
}

func newOperation(name string) (*operation, error) {
	meter := otel.Meter("fabric/control/inventory")
	count, err := meter.Int64Counter("fabric.inventory."+name+".operations", metric.WithUnit("{operation}"))
	if err != nil {
		return nil, err
	}
	duration, err := meter.Float64Histogram("fabric.inventory."+name+".duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return &operation{count: count, duration: duration, tracer: otel.Tracer("fabric/control/inventory"), name: name}, nil
}
func (o *operation) start(ctx context.Context) (context.Context, func(error)) {
	if o == nil {
		return ctx, func(error) {}
	}
	ctx, span := o.tracer.Start(ctx, "inventory."+o.name)
	start := time.Now()
	return ctx, func(err error) {
		result := "success"
		if err != nil {
			result = "error"
			span.RecordError(err)
			span.SetStatus(codes.Error, "inventory operation failed")
		}
		attrs := metric.WithAttributes(attribute.String("result", result))
		o.count.Add(ctx, 1, attrs)
		o.duration.Record(ctx, time.Since(start).Seconds(), attrs)
		span.End()
	}
}
