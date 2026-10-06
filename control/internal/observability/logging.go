package observability

import (
	"context"
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/trace"
)

type handler struct {
	console slog.Handler
	export  slog.Handler
	level   slog.Level
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool { return level >= h.level }
func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	local := r.Clone()
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		local.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return errors.Join(h.console.Handle(ctx, local), h.export.Handle(ctx, r))
}
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &handler{console: h.console.WithAttrs(attrs), export: h.export.WithAttrs(attrs), level: h.level}
}
func (h *handler) WithGroup(name string) slog.Handler {
	return &handler{console: h.console.WithGroup(name), export: h.export.WithGroup(name), level: h.level}
}
