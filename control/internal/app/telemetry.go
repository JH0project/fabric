package app

import (
	"context"
	"errors"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type instruments struct {
	events           metric.Int64Counter
	snapshots        metric.Int64Counter
	inspects         metric.Int64Counter
	snapshotDuration metric.Float64Histogram
	inspectDuration  metric.Float64Histogram
}

func (a *App) instruments(meter metric.Meter) (metric.Registration, error) {
	var err error
	m := &instruments{}
	if m.events, err = meter.Int64Counter("fabric.runtime.events", metric.WithUnit("{event}")); err != nil {
		return nil, err
	}
	if m.snapshots, err = meter.Int64Counter("fabric.runtime.snapshots", metric.WithUnit("{operation}")); err != nil {
		return nil, err
	}
	if m.inspects, err = meter.Int64Counter("fabric.runtime.inspects", metric.WithUnit("{operation}")); err != nil {
		return nil, err
	}
	if m.snapshotDuration, err = meter.Float64Histogram("fabric.runtime.snapshot.duration", metric.WithUnit("s")); err != nil {
		return nil, err
	}
	if m.inspectDuration, err = meter.Float64Histogram("fabric.runtime.inspect.duration", metric.WithUnit("s")); err != nil {
		return nil, err
	}
	gauge, err := meter.Int64ObservableGauge("fabric.runtime.containers", metric.WithUnit("{container}"))
	if err != nil {
		return nil, err
	}
	a.telemetry = m
	return meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		counts := map[string]int64{}
		for _, c := range a.View.Snapshot() {
			counts[stateLabel(c.State)]++
		}
		for _, state := range containerStates {
			o.ObserveInt64(gauge, counts[state], metric.WithAttributes(attribute.String("container.state", state)))
		}
		return nil
	}, gauge)
}

var containerStates = []string{"created", "running", "paused", "restarting", "removing", "exited", "dead", "unknown"}

func stateLabel(state string) string {
	for _, known := range containerStates {
		if state == known {
			return state
		}
	}
	return "unknown"
}
func actionLabel(action string) string {
	if strings.HasPrefix(action, "health_status:") {
		return "health_status"
	}
	if strings.HasPrefix(action, "exec_") {
		return "exec"
	}
	switch action {
	case "create", "start", "stop", "die", "destroy", "pause", "unpause", "restart", "rename", "update", "oom", "kill":
		return action
	}
	return "other"
}

func complete(ctx context.Context, span trace.Span, start time.Time, err error, counter metric.Int64Counter, duration metric.Float64Histogram) {
	result := "success"
	if errors.Is(err, context.Canceled) {
		result = "cancelled"
	} else if err != nil {
		result = "error"
		span.RecordError(err)
		span.SetStatus(codes.Error, "Docker observation failed")
	}
	attrs := metric.WithAttributes(attribute.String("result", result))
	counter.Add(ctx, 1, attrs)
	duration.Record(ctx, time.Since(start).Seconds(), attrs)
	span.End()
}
