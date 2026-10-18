package configuration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTemplateNodeSpecificCorpus(t *testing.T) {
	nodes := []any{
		map[string]any{"id": "node-a", "name": "region-a-node-1", "ip": "192.0.2.1", "region": "region-a"},
		map[string]any{"id": "node-b", "name": "region-a-node-2", "ip": "192.0.2.2", "region": "region-a"},
		map[string]any{"id": "node-c", "name": "region-b-node-1", "ip": "192.0.2.3", "region": "region-b"},
		map[string]any{"id": "node-d", "name": "region-b-node-2", "ip": "192.0.2.4", "region": "region-b"},
		map[string]any{"id": "node-e", "name": "region-c-node-1", "ip": "192.0.2.5", "region": "region-c"},
		map[string]any{"id": "node-f", "name": "region-c-node-2", "ip": "192.0.2.6", "region": "region-c"},
	}
	for _, n := range nodes {
		node := n.(map[string]any)
		data := map[string]any{"node": node, "nodes": nodes}
		rendered, err := RenderTemplate(context.Background(), TemplateSpec{
			Engine: "go-template/v1",
			Source: "active-replica yes\nmulti-master yes\n{{range .nodes}}{{if ne .id $.node.id}}replicaof {{.ip}} 6379\n{{end}}{{end}}",
			Data:   data,
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Count(rendered, "replicaof ") != 5 || strings.Contains(rendered, "replicaof "+node["ip"].(string)+" ") {
			t.Fatalf("incorrect peer selection: %s", rendered)
		}
		for _, tc := range []struct{ source, want string }{
			{"external_labels:\n  instance: {{.node.name}}\n  region: {{.node.region}}\n", "instance: " + node["name"].(string)},
			{"instance_id: {{.node.name}}\n", "instance_id: " + node["name"].(string)},
		} {
			actual, err := RenderTemplate(context.Background(), TemplateSpec{Engine: "go-template/v1", Source: tc.source, Data: data})
			if err != nil || !strings.Contains(actual, tc.want) {
				t.Fatal(actual, err)
			}
		}
	}
}

func TestTemplateStrictAndBounded(t *testing.T) {
	cases := []struct {
		name, source string
		data         map[string]any
		want         error
	}{
		{"missing", "{{.absent}}", map[string]any{}, ErrTemplateInvalid},
		{"format allocation", `{{printf "%1000000000s" "secret-marker"}}`, nil, ErrTemplateInvalid},
		{"call", `{{call .function}}`, nil, ErrTemplateInvalid},
		{"environment", `{{env "HOME"}}`, nil, ErrTemplateInvalid},
		{"recursion", `{{template "file" .}}`, nil, ErrTemplateInvalid},
		{"definition", `{{define "other"}}x{{end}}x`, nil, ErrTemplateInvalid},
		{"invisible loop", "{{range 100001}}{{end}}", nil, ErrTemplateLimit},
		{"nested loop", "{{range 400}}{{range 400}}{{end}}{{end}}", nil, ErrTemplateLimit},
		{"output limit", "{{range 2000}}" + strings.Repeat("x", 4096) + "{{end}}", nil, ErrTemplateLimit},
		{"source limit", strings.Repeat("x", MaxTemplateBytes+1), nil, ErrTemplateLimit},
		{"callable data", "{{.x}}", map[string]any{"x": func() string { return "secret-marker" }}, ErrTemplateInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, err := RenderTemplate(context.Background(), TemplateSpec{Engine: "go-template/v1", Source: tc.source, Data: tc.data})
			if !errors.Is(err, tc.want) || text != "" || strings.Contains(err.Error(), "secret-marker") {
				t.Fatal(text, err)
			}
		})
	}
}

func TestTemplateTextAndCancellation(t *testing.T) {
	for _, want := range []string{"", "0", "<>&\"", "{{literal}}"} {
		text, err := RenderTemplate(context.Background(), TemplateSpec{Engine: "go-template/v1", Source: "{{.value}}", Data: map[string]any{"value": want}})
		if err != nil || text != want {
			t.Fatal(text, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RenderTemplate(ctx, TemplateSpec{Engine: "go-template/v1", Source: "x"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := RenderTemplate(context.Background(), TemplateSpec{Engine: "jinja", Source: "x"}); !errors.Is(err, ErrTemplateInvalid) {
		t.Fatal(err)
	}
}

func TestTemplateUsesCallerEnvironmentList(t *testing.T) {
	t.Setenv("CONFIG_VALUE", "agent-host-value")
	source := `{{range .env}}{{if eq .key "CONFIG_VALUE"}}{{.value}}{{end}}{{end}}`
	for _, want := range []string{"", "0", "quoted \"value\"\nline", "{{.node.name}}"} {
		rendered, err := RenderTemplate(context.Background(), TemplateSpec{
			Engine: "go-template/v1", Source: source, Data: map[string]any{
				"env":   []any{map[string]any{"key": "CONFIG_VALUE", "value": want}},
				"node":  map[string]any{"name": "must-not-expand"},
				"nodes": []any{},
			},
		})
		if err != nil || rendered != want {
			t.Fatal(rendered, err)
		}
	}
	rendered, err := RenderTemplate(context.Background(), TemplateSpec{Engine: "go-template/v1", Source: "", Data: map[string]any{"env": []any{}, "nodes": []any{}}})
	if err != nil || rendered != "" {
		t.Fatal(rendered, err)
	}
}
