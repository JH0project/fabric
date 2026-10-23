package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func base() Request {
	return Request{SchemaVersion: 1, ContainerName: "app-20261023", ServiceName: "app-20261023", Image: "example/app:commit", Network: "coolify", Restart: "unless-stopped", Labels: []string{"coolify.managed=true"}, Aliases: []string{"app"}, Expose: []any{json.Number("8080")}, Limits: map[string]any{"cpus": json.Number("0.5"), "mem_limit": "512m"}}
}
func document(t *testing.T, r Request) map[string]any {
	t.Helper()
	out, err := Generate(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(out.Compose))
	if out.SHA256 != hex.EncodeToString(sum[:]) || out.SchemaVersion != 1 {
		t.Fatal("invalid receipt")
	}
	var doc map[string]any
	if err = yaml.Unmarshal([]byte(out.Compose), &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}
func service(t *testing.T, r Request) map[string]any {
	return document(t, r)["services"].(map[string]any)[r.ServiceName].(map[string]any)
}
func TestGenerateRuntimeOptions(t *testing.T) {
	r := base()
	grace := "700s"
	r.StopGracePeriod = &grace
	r.Healthcheck = map[string]any{"test": []any{"CMD-SHELL", "curl -f http://localhost:8080/ || exit 1"}, "retries": 3}
	r.Volumes = []string{"data:/app/data", "/data/coolify/config:/etc/app/config:ro"}
	r.NamedVolumes = map[string]any{"data": map[string]any{"name": "data", "external": false}}
	r.Ports = []any{"127.0.0.1:8080:8080"}
	r.Logging = map[string]any{"driver": "fluentd"}
	r.GPU = map[string]any{"driver": "nvidia", "capabilities": []any{"gpu"}, "count": "all", "options": map[string]any{}}
	r.CustomOptions = map[string]any{"user": "1000:1000", "group_add": []any{"123"}, "entrypoint": "/bin/echo hello world"}
	s := service(t, r)
	if s["container_name"] != r.ContainerName || s["stop_grace_period"] != "700s" || s["user"] != "1000:1000" || s["entrypoint"] != "/bin/echo hello world" {
		t.Fatal(s)
	}
	if len(s["volumes"].([]any)) != 2 || len(s["ports"].([]any)) != 1 || s["healthcheck"].(map[string]any)["retries"] != 3 {
		t.Fatal(s)
	}
	if s["logging"].(map[string]any)["driver"] != "fluentd" || s["deploy"] == nil || document(t, r)["volumes"] == nil {
		t.Fatal(s)
	}
}
func TestPreviewDoesNotApplyProductionOptions(t *testing.T) {
	r := base()
	r.Preview = true
	r.Ports = []any{"80:8080"}
	r.CustomOptions = map[string]any{"privileged": true, "ip": []any{"10.0.0.5"}}
	s := service(t, r)
	for _, key := range []string{"ports", "privileged"} {
		if _, ok := s[key]; ok {
			t.Fatal("production option leaked", key)
		}
	}
}
func TestStaticIPAliasesMatchLegacyNamingModes(t *testing.T) {
	for _, consistent := range []bool{true, false} {
		t.Run(map[bool]string{true: "consistent", false: "generated"}[consistent], func(t *testing.T) {
			r := base()
			r.ConsistentName = consistent
			if consistent {
				r.ServiceName = "application-uuid"
			}
			r.CustomOptions = map[string]any{"ip": []any{"10.0.0.25"}, "ip6": []any{"2001:db8::25"}}
			s := service(t, r)
			network := s["networks"].(map[string]any)["coolify"].(map[string]any)
			if network["ipv4_address"] != "10.0.0.25" || network["ipv6_address"] != "2001:db8::25" {
				t.Fatal(network)
			}
			_, hasAliases := network["aliases"]
			if hasAliases != consistent {
				t.Fatal("alias behavior changed", network)
			}
		})
	}
}
func TestSwarmAndGPU(t *testing.T) {
	r := base()
	r.Swarm = &Swarm{Replicas: 3, CPUs: "0.5", Memory: "512m", WorkerOnly: true, Overrides: map[string]any{"placement": map[string]any{"constraints": []any{"node.labels.zone == jp"}}}}
	r.GPU = map[string]any{"driver": "nvidia", "capabilities": []any{"gpu"}, "ids": []any{"gpu-0"}}
	before, _ := json.Marshal(r)
	s := service(t, r)
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("input mutated")
	}
	for _, key := range []string{"container_name", "expose", "restart", "cpus", "mem_limit"} {
		if _, ok := s[key]; ok {
			t.Fatal("swarm option leaked", key)
		}
	}
	d := s["deploy"].(map[string]any)
	if d["replicas"] != 3 || len(d["placement"].(map[string]any)["constraints"].([]any)) != 2 {
		t.Fatal(d)
	}
	r.Preview = true
	if service(t, r)["deploy"].(map[string]any)["replicas"] != 1 {
		t.Fatal("preview replicas")
	}
}
func TestRecursiveCustomMerge(t *testing.T) {
	r := base()
	r.CustomOptions = map[string]any{"labels": []any{"custom=value"}, "restart": "always", "networks": map[string]any{"coolify": map[string]any{"aliases": []any{"second"}}}}
	s := service(t, r)
	if !reflect.DeepEqual(s["restart"], []any{"unless-stopped", "always"}) || len(s["labels"].([]any)) != 2 || len(s["networks"].(map[string]any)["coolify"].(map[string]any)["aliases"].([]any)) != 3 {
		t.Fatal(s)
	}
}
func TestValidationCancellationAndDeterminism(t *testing.T) {
	for _, change := range []func(*Request){func(r *Request) { r.SchemaVersion = 2 }, func(r *Request) { r.Network = "bad/network" }, func(r *Request) { r.ServiceName = "" }, func(r *Request) { r.Image = "" }, func(r *Request) { r.Limits["unknown"] = true }, func(r *Request) { r.Preview = true; r.ServiceName = "wrong" }} {
		r := base()
		change(&r)
		if _, err := Generate(context.Background(), r); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Generate(ctx, base()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	a, _ := Generate(context.Background(), base())
	b, _ := Generate(context.Background(), base())
	if a != b {
		t.Fatal("unstable output")
	}
	r := base()
	r.Labels = []string{strings.Repeat("x", MaxDocumentBytes)}
	if _, err := Generate(context.Background(), r); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized output", err)
	}
	r = base()
	r.Limits["memswap_limit"] = json.Number("9007199254740993")
	if service(t, r)["memswap_limit"] != 9007199254740993 {
		t.Fatal("integer precision lost")
	}
}
