package rpc

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JH0project/fabric/control/internal/compose"
)

func TestComposeStatelessRPC(t *testing.T) {
	// No file applier, database, Docker or filesystem roots are needed.
	s, err := New("127.0.0.1:0", testToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"schema_version":1,"container_name":"app","service_name":"app","image":"example/app:latest","network":"coolify","restart":"unless-stopped","labels":[],"limits":{},"custom_options":{}}`
	if w := request(s, "POST", "/v1/compose/generate", body, "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(s, "GET", "/v1/compose/generate", "", testToken); w.Code != 405 {
		t.Fatal(w.Code)
	}
	w := request(s, "POST", "/v1/compose/generate", body, testToken)
	var out compose.Result
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || !strings.Contains(out.Compose, "example/app:latest") || out.SHA256 == "" {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, invalid := range []string{`null`, `{}`, body + ` {}`, strings.Replace(body, `"limits":{}`, `"limits":{},"unknown":true`, 1), strings.Replace(body, `"schema_version":1`, `"schema_version":2`, 1), strings.Replace(body, `"limits":{}`, `"limits":[]`, 1)} {
		w = request(s, "POST", "/v1/compose/generate", invalid, testToken)
		if w.Code != 400 && w.Code != 422 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w = request(s, "POST", "/v1/compose/generate", strings.Repeat(" ", MaxRequestBytes+1), testToken); w.Code != 413 {
		t.Fatal(w.Code)
	}
	if w = request(s, "POST", "/v1/compose/generate", body+strings.Repeat(" ", MaxRequestBytes), testToken); w.Code != 413 {
		t.Fatal(w.Code)
	}
}

// Start only for cross-language tests. It exposes no Docker, file or database service.
func TestComposeHTTPIntegrationServer(t *testing.T) {
	if os.Getenv("FABRIC_COMPOSE_TEST_SERVER") != "true" {
		t.Skip("opt-in cross-language test server")
	}
	s, err := New("127.0.0.1:0", strings.Repeat("a", 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var once sync.Once
	handler := s.Handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/test/shutdown" && r.Method == "POST" && r.Header.Get("Authorization") == "Bearer "+strings.Repeat("a", 32) {
			w.WriteHeader(204)
			once.Do(func() { close(done) })
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	fmt.Println("COMPOSE_TEST_ENDPOINT=" + server.URL)
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("cross-language test timed out")
	}
}
func TestConfigurationDisabledWithoutApplier(t *testing.T) {
	s, _ := New("127.0.0.1:0", testToken, nil)
	if w := request(s, "POST", "/v1/configurations/apply", `{}`, testToken); w.Code != 503 {
		t.Fatal(w.Code)
	}
}
