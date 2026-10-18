package configuration

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"log/slog"
	"time"
)

type FileWriter interface {
	Write(context.Context, File) (string, error)
}
type Request struct {
	ServiceID string `json:"service_id"`
	Files     []File `json:"files"`
}
type FileResult struct {
	Path    string `json:"path"`
	Applied bool   `json:"applied"`
	SHA256  string `json:"sha256,omitempty"`
	Code    string `json:"code,omitempty"`
}
type Result struct {
	Applied bool         `json:"applied"`
	Files   []FileResult `json:"files"`
}
type Service struct {
	writer   FileWriter
	logger   *slog.Logger
	count    metric.Int64Counter
	duration metric.Float64Histogram
}

func NewService(writer FileWriter, logger *slog.Logger) (*Service, error) {
	meter := otel.Meter("github.com/JH0project/fabric/control/configuration")
	count, err := meter.Int64Counter("fabric.configuration.apply", metric.WithUnit("{operation}"))
	if err != nil {
		return nil, err
	}
	duration, err := meter.Float64Histogram("fabric.configuration.apply.duration", metric.WithUnit("s"))
	if err != nil {
		return nil, err
	}
	return &Service{writer, logger, count, duration}, nil
}
func (s *Service) Apply(ctx context.Context, request Request) Result {
	ctx, span := otel.Tracer("github.com/JH0project/fabric/control/configuration").Start(ctx, "configuration.apply")
	start := time.Now()
	result := Result{Applied: true, Files: make([]FileResult, 0, len(request.Files))}
	outcome := "success"
	files, rendered := prepare(ctx, request.Files)
	if !rendered.Rendered {
		result.Applied = false
		outcome = "error"
		for _, receipt := range rendered.Files {
			code := receipt.Code
			if code == "" {
				code = "not_applied"
			}
			if code == "cancelled" {
				outcome = "cancelled"
			}
			result.Files = append(result.Files, FileResult{Path: receipt.Path, Code: code})
		}
	} else {
		for _, file := range files {
			digest, err := s.writer.Write(ctx, file)
			item := FileResult{Path: file.Path, Applied: err == nil, SHA256: digest}
			if err != nil {
				result.Applied = false
				outcome = "error"
				item.Code = errorCode(err)
				if errors.Is(err, context.Canceled) {
					outcome = "cancelled"
				}
			}
			result.Files = append(result.Files, item)
		}
	}
	attributes := metric.WithAttributes(attribute.String("result", outcome))
	s.count.Add(ctx, 1, attributes)
	s.duration.Record(ctx, time.Since(start).Seconds(), attributes)
	if !result.Applied && outcome != "cancelled" {
		span.SetStatus(codes.Error, "configuration application failed")
	}
	if outcome == "error" {
		s.logger.ErrorContext(ctx, "configuration application failed", "service.id", request.ServiceID, "result", outcome, "files", len(result.Files))
	} else {
		s.logger.InfoContext(ctx, "configuration application completed", "service.id", request.ServiceID, "result", outcome, "files", len(result.Files))
	}
	span.End()
	return result
}
