package rpc

import (
	"context"
	"encoding/json"
	"github.com/JH0project/fabric/control/internal/configuration"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "01234567890123456789012345678901"

func setup(t *testing.T) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	writer, err := configuration.NewWriter([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writer.Close() })
	service, err := configuration.NewService(writer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	server, err := New("127.0.0.1:0", testToken, service)
	if err != nil {
		t.Fatal(err)
	}
	return server, dir
}
func request(s *Server, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}
func TestApplyAndAuthentication(t *testing.T) {
	s, dir := setup(t)
	path := filepath.Join(dir, "config")
	body, _ := json.Marshal(map[string]any{"service_id": "app", "files": []any{map[string]any{"path": path, "content": "0"}}})
	if w := request(s, "POST", "/v1/configurations/apply", string(body), "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unauthorized write")
	}
	w := request(s, "POST", "/v1/configurations/apply", string(body), testToken)
	var result configuration.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || !result.Applied || len(result.Files) != 1 || result.Files[0].SHA256 == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	data, _ := os.ReadFile(path)
	if string(data) != "0" {
		t.Fatal("content lost")
	}
}
func TestInvalidRequests(t *testing.T) {
	s, dir := setup(t)
	path, _ := json.Marshal(filepath.Join(dir, "file"))
	bodies := []string{
		`{"service_id":"app","files":[]}`,
		`{"service_id":"app","files":[{"path":` + string(path) + `}]} `,
		`{"service_id":"app","files":[{"path":` + string(path) + `,"content":null}]}`,
		`{"service_id":"app","files":[{"path":` + string(path) + `,"content":"x","extra":true}]}`,
		`{"service_id":"app","files":[{"path":` + string(path) + `,"content":""}]} {}`,
	}
	for _, body := range bodies {
		if w := request(s, "POST", "/v1/configurations/apply", body, testToken); w.Code != 400 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request(s, "GET", "/v1/configurations/apply", "", testToken); w.Code != 405 {
		t.Fatal(w.Code)
	}
	if w := request(s, "POST", "/unknown", "{}", testToken); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := request(s, "POST", "/v1/configurations/apply", strings.Repeat(" ", MaxRequestBytes+1), testToken); w.Code != 413 {
		t.Fatal(w.Code)
	}
}
func TestPartialResultsAndConflicts(t *testing.T) {
	s, dir := setup(t)
	conflict := filepath.Join(dir, "directory")
	os.Mkdir(conflict, 0755)
	body, _ := json.Marshal(map[string]any{"service_id": "app", "files": []any{
		map[string]any{"path": filepath.Join(dir, "good"), "content": ""},
		map[string]any{"path": conflict, "content": "secret-never-in-errors"},
	}})
	w := request(s, http.MethodPost, "/v1/configurations/apply", string(body), testToken)
	var result configuration.Result
	json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 207 || result.Applied || !result.Files[0].Applied || result.Files[1].Code != "path_conflict" {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "secret-never-in-errors") {
		t.Fatal("content leaked")
	}
}
func TestListenFailureAndCancelledRun(t *testing.T) {
	s, _ := setup(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.Address = listener.Addr().String()
	if err := s.Run(context.Background()); err == nil {
		t.Fatal("ignored listen failure")
	}
	listener.Close()
	s.Address = "127.0.0.1:0"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown stalled")
	}
	if _, err := New("127.0.0.1:0", "short", s.applier); err == nil {
		t.Fatal("accepted weak token")
	}
}
