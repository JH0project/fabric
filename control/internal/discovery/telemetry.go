package discovery

import (
	"context"
	"time"

	"github.com/miekg/dns"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type dnsTelemetry struct {
	queries  metric.Int64Counter
	duration metric.Float64Histogram
	tracer   trace.Tracer
}

func newDNSTelemetry() (*dnsTelemetry, error) {
	meter := otel.Meter("fabric/control/discovery")
	queries, err := meter.Int64Counter("fabric.dns.queries", metric.WithUnit("{query}"))
	if err != nil {
		return nil, err
	}
	duration, err := meter.Float64Histogram("fabric.dns.query.duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return &dnsTelemetry{queries: queries, duration: duration, tracer: otel.Tracer("fabric/control/discovery")}, nil
}

func (t *dnsTelemetry) start(request *dns.Msg) func(int) {
	if t == nil {
		return func(int) {}
	}
	qtype := "other"
	if len(request.Question) == 1 {
		switch request.Question[0].Qtype {
		case dns.TypeA:
			qtype = "A"
		case dns.TypeAAAA:
			qtype = "AAAA"
		case dns.TypeSRV:
			qtype = "SRV"
		case dns.TypeSOA:
			qtype = "SOA"
		}
	}
	ctx, span := t.tracer.Start(context.Background(), "dns.query", trace.WithAttributes(attribute.String("dns.question.type", qtype)))
	start := time.Now()
	return func(code int) {
		attrs := []attribute.KeyValue{attribute.String("dns.question.type", qtype), attribute.String("dns.response.code", dns.RcodeToString[code])}
		span.SetAttributes(attrs...)
		t.queries.Add(ctx, 1, metric.WithAttributes(attrs...))
		t.duration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		span.End()
	}
}
