package rpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JH0project/fabric/control/internal/configuration"
	"github.com/JH0project/fabric/control/internal/environment"
)

type rpcEnvironment struct{ calls int }

func (e *rpcEnvironment) Resolve(context.Context, environment.Reference) ([]environment.Variable, error) {
	e.calls++
	return []environment.Variable{{Key: "KEY", Value: "synthetic-private-value"}}, nil
}

func TestDatabaseEnvironmentRPCAuthenticatesAndReturnsReceiptsOnly(t *testing.T) {
	s, dir := setup(t)
	source := &rpcEnvironment{}
	s.applier.(*configuration.Service).Environments = source
	file := map[string]any{"path": filepath.Join(dir, "db-rendered"), "template": map[string]any{
		"engine": "go-template/v1", "source": "{{range .env}}{{.value}}{{end}}",
		"environment": map[string]any{"application_uuid": "app", "server_id": 2, "team_id": 1},
		"data":        map[string]any{"node": map[string]any{"id": "2"}},
	}}
	body, _ := json.Marshal(map[string]any{"service_id": "app", "files": []any{file}})
	if got := request(s, "POST", "/v1/configurations/render", string(body), "wrong"); got.Code != 401 || source.calls != 0 {
		t.Fatal("lookup before authentication")
	}
	got := request(s, "POST", "/v1/configurations/render", string(body), testToken)
	if got.Code != 200 || source.calls != 1 || strings.Contains(got.Body.String(), "synthetic-private-value") {
		t.Fatal("unsafe database render response", got.Code)
	}
	var receipt configuration.RenderResult
	if json.Unmarshal(got.Body.Bytes(), &receipt) != nil || !receipt.Rendered {
		t.Fatal("invalid receipt")
	}
	file["expected_sha256"] = receipt.Files[0].SHA256
	body, _ = json.Marshal(map[string]any{"service_id": "app", "files": []any{file}})
	if got := request(s, "POST", "/v1/configurations/apply", string(body), testToken); got.Code != 200 || source.calls != 2 {
		t.Fatal("database apply failed", got.Code)
	}
	text, err := os.ReadFile(filepath.Join(dir, "db-rendered"))
	if err != nil || string(text) != "synthetic-private-value" {
		t.Fatal("incorrect rendered file")
	}
}

func TestTemplateRenderPreflightAndApply(t *testing.T) {
	s, dir := setup(t)
	path := filepath.Join(dir, "rendered")
	source := "{{range .nodes}}{{if ne .id $.node.id}}replicaof {{.ip}} 6379\n{{end}}{{end}}"
	file := map[string]any{"path": path, "template": map[string]any{
		"engine": "go-template/v1", "source": source,
		"data": map[string]any{"node": map[string]any{"id": "one"}, "nodes": []any{
			map[string]any{"id": "one", "ip": "192.0.2.1"}, map[string]any{"id": "two", "ip": "192.0.2.2"},
		}},
	}}
	body := func() string {
		b, _ := json.Marshal(map[string]any{"service_id": "app", "files": []any{file}})
		return string(b)
	}
	if w := request(s, "POST", "/v1/configurations/render", body(), "wrong"); w.Code != 401 {
		t.Fatal(w.Code)
	}
	w := request(s, "POST", "/v1/configurations/render", body(), testToken)
	var rendered struct {
		Rendered bool `json:"rendered"`
		Files    []struct {
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	sum := sha256.Sum256([]byte("replicaof 192.0.2.2 6379\n"))
	expected := hex.EncodeToString(sum[:])
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &rendered) != nil || !rendered.Rendered || rendered.Files[0].SHA256 != expected {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("preflight wrote a file")
	}
	if strings.Contains(w.Body.String(), "192.0.2.2") {
		t.Fatal("rendered content leaked in receipt")
	}
	file["expected_sha256"] = strings.Repeat("0", 64)
	if w := request(s, "POST", "/v1/configurations/apply", body(), testToken); w.Code != 207 || !strings.Contains(w.Body.String(), "digest_mismatch") {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("digest mismatch wrote a file")
	}
	file["expected_sha256"] = expected
	if w := request(s, "POST", "/v1/configurations/apply", body(), testToken); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "replicaof 192.0.2.2 6379\n" {
		t.Fatal(string(data), err)
	}
}

func TestBadTemplatePreventsBatchWrites(t *testing.T) {
	s, dir := setup(t)
	path := filepath.Join(dir, "literal")
	body, _ := json.Marshal(map[string]any{"service_id": "app", "files": []any{
		map[string]any{"path": path, "content": "unchanged"},
		map[string]any{"path": filepath.Join(dir, "bad"), "template": map[string]any{"engine": "go-template/v1", "source": "secret-marker {{.missing}}", "data": map[string]any{}}},
	}})
	for endpoint, status := range map[string]int{"/v1/configurations/render": 422, "/v1/configurations/apply": 207} {
		w := request(s, "POST", endpoint, string(body), testToken)
		if w.Code != status || !strings.Contains(w.Body.String(), "invalid_template") || strings.Contains(w.Body.String(), "secret-marker") {
			t.Fatal(w.Code, w.Body.String())
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("batch wrote before successful render")
		}
	}
}
func TestTemplateAndContentAreExclusive(t *testing.T) {
	s, dir := setup(t)
	body, _ := json.Marshal(map[string]any{"service_id": "app", "files": []any{
		map[string]any{"path": filepath.Join(dir, "bad"), "content": "literal", "template": map[string]any{"engine": "go-template/v1", "source": "rendered"}},
	}})
	if w := request(s, "POST", "/v1/configurations/apply", string(body), testToken); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
}
