// Package rpc provides authenticated HTTP adapters for agent services.
package rpc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"github.com/JH0project/fabric/control/internal/configuration"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"io"
	"net"
	"net/http"
	"time"
)

const MaxRequestBytes = 8 * 1024 * 1024

type Applier interface {
	Apply(context.Context, configuration.Request) configuration.Result
}
type Renderer interface {
	Render(context.Context, configuration.Request) configuration.RenderResult
}

type Server struct {
	Address string
	token   string
	applier Applier
}

func New(address, token string, applier Applier) (*Server, error) {
	if len(token) < 32 {
		return nil, errors.New("RPC token must contain at least 32 bytes")
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return nil, errors.New("RPC listen address must contain host and port")
	}
	return &Server{address, token, applier}, nil
}
func respond(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		operation := "rpc.configuration.apply"
		if r.URL.Path == "/v1/configurations/render" {
			operation = "rpc.configuration.render"
		}
		ctx, span := otel.Tracer("github.com/JH0project/fabric/control/rpc").Start(ctx, operation)
		defer span.End()
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
			respond(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if r.URL.Path != "/v1/configurations/apply" && r.URL.Path != "/v1/configurations/render" {
			respond(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			respond(w, http.StatusMethodNotAllowed, map[string]string{"error": "method_not_allowed"})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request configuration.Request
		if err := decoder.Decode(&request); err != nil {
			status := http.StatusBadRequest
			var large *http.MaxBytesError
			if errors.As(err, &large) {
				status = http.StatusRequestEntityTooLarge
			}
			respond(w, status, map[string]string{"error": "invalid_request"})
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF || request.ServiceID == "" || len(request.ServiceID) > 128 || len(request.Files) < 1 || len(request.Files) > 32 {
			respond(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		paths := make(map[string]bool)
		for _, file := range request.Files {
			if !file.ValidInput() || paths[file.Path] {
				respond(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
				return
			}
			paths[file.Path] = true
		}
		if r.URL.Path == "/v1/configurations/render" {
			renderer, ok := s.applier.(Renderer)
			if !ok {
				respond(w, http.StatusNotImplemented, map[string]string{"error": "render_not_supported"})
				return
			}
			result := renderer.Render(ctx, request)
			status := http.StatusOK
			if !result.Rendered {
				span.SetStatus(codes.Error, "configuration rendering failed")
				status = http.StatusUnprocessableEntity
			}
			respond(w, status, result)
			return
		}
		result := s.applier.Apply(ctx, request)
		status := http.StatusOK
		if !result.Applied {
			status = http.StatusMultiStatus
			if !errors.Is(ctx.Err(), context.Canceled) {
				span.SetStatus(codes.Error, "configuration application failed")
			}
		}
		respond(w, status, result)
	})
}
func (s *Server) Run(ctx context.Context) error {
	listener, err := net.Listen("tcp", s.Address)
	if err != nil {
		return err
	}
	server := &http.Server{
		Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownCtx)
		if err != nil {
			_ = server.Close()
		}
		serveErr := <-done
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		return errors.Join(err, serveErr)
	}
}
