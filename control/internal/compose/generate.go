// Package compose generates deployment documents without accessing Docker, files or databases.
package compose

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"

	"go.yaml.in/yaml/v3"
)

const MaxDocumentBytes = 8 * 1024 * 1024

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
var ErrInvalid = errors.New("invalid compose specification")

type Request struct {
	SchemaVersion   int            `json:"schema_version"`
	ContainerName   string         `json:"container_name"`
	ServiceName     string         `json:"service_name"`
	Image           string         `json:"image"`
	Network         string         `json:"network"`
	Restart         string         `json:"restart"`
	Labels          []string       `json:"labels"`
	Aliases         []string       `json:"aliases"`
	Expose          []any          `json:"expose"`
	Limits          map[string]any `json:"limits"`
	StopGracePeriod *string        `json:"stop_grace_period"`
	Healthcheck     map[string]any `json:"healthcheck"`
	Swarm           *Swarm         `json:"swarm"`
	Logging         map[string]any `json:"logging"`
	GPU             map[string]any `json:"gpu"`
	Ports           []any          `json:"ports"`
	Volumes         []string       `json:"volumes"`
	NamedVolumes    map[string]any `json:"named_volumes"`
	CustomOptions   map[string]any `json:"custom_options"`
	Preview         bool           `json:"preview"`
	ConsistentName  bool           `json:"consistent_name"`
}

type Swarm struct {
	Replicas   int            `json:"replicas"`
	CPUs       any            `json:"cpus"`
	Memory     any            `json:"memory"`
	Overrides  map[string]any `json:"overrides"`
	WorkerOnly bool           `json:"worker_only"`
}

type Result struct {
	SchemaVersion int    `json:"schema_version"`
	Compose       string `json:"compose"`
	SHA256        string `json:"sha256"`
}

func Generate(ctx context.Context, r Request) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if r.SchemaVersion != 1 || !namePattern.MatchString(r.ContainerName) || !namePattern.MatchString(r.ServiceName) || !namePattern.MatchString(r.Network) || r.Image == "" || len(r.Image) > 4096 || (r.Preview && r.ServiceName != r.ContainerName) {
		return Result{}, ErrInvalid
	}
	for k := range r.Limits {
		switch k {
		case "mem_limit", "memswap_limit", "mem_swappiness", "mem_reservation", "cpus", "cpu_shares", "cpuset":
		default:
			return Result{}, ErrInvalid
		}
	}
	aliases := append([]string{r.ContainerName}, r.Aliases...)
	network := map[string]any{"aliases": aliases}
	service := map[string]any{
		"image": r.Image, "container_name": r.ContainerName, "restart": r.Restart,
		"networks": map[string]any{r.Network: network}, "env_file": []string{".env"},
	}
	for k, v := range r.Limits {
		service[k] = v
	}
	if len(r.Expose) > 0 {
		service["expose"] = r.Expose
	}
	if r.StopGracePeriod != nil {
		service["stop_grace_period"] = *r.StopGracePeriod
	}
	if len(r.Healthcheck) > 0 {
		service["healthcheck"] = cloneMap(r.Healthcheck)
	}
	if r.Swarm != nil {
		for _, k := range []string{"container_name", "expose", "restart", "mem_limit", "memswap_limit", "mem_swappiness", "mem_reservation", "cpus", "cpuset", "cpu_shares"} {
			delete(service, k)
		}
		resources := map[string]any{"cpus": r.Swarm.CPUs, "memory": r.Swarm.Memory}
		replicas := r.Swarm.Replicas
		if r.Preview {
			replicas = 1
		}
		deploy := map[string]any{
			"mode": "replicated", "replicas": replicas,
			"update_config":   map[string]any{"order": "start-first"},
			"rollback_config": map[string]any{"order": "start-first"},
			"labels":          r.Labels,
			"resources":       map[string]any{"limits": resources, "reservations": cloneMap(resources)},
		}
		// Legacy array_merge replaces an entire top-level deploy option.
		for k, v := range r.Swarm.Overrides {
			deploy[k] = cloneValue(v)
		}
		if r.Swarm.WorkerOnly {
			placement, _ := deploy["placement"].(map[string]any)
			if placement == nil {
				placement = map[string]any{}
			}
			constraints, _ := placement["constraints"].([]any)
			placement["constraints"] = append(constraints, "node.role == worker")
			deploy["placement"] = placement
		}
		if r.Preview {
			deploy["replicas"] = 1
		}
		service["deploy"] = deploy
	} else {
		service["labels"] = r.Labels
	}
	if len(r.Logging) > 0 {
		service["logging"] = r.Logging
	}
	if len(r.GPU) > 0 {
		deploy, _ := service["deploy"].(map[string]any)
		if deploy == nil {
			deploy = map[string]any{}
		}
		resources, _ := deploy["resources"].(map[string]any)
		if resources == nil {
			resources = map[string]any{}
		}
		reservations, _ := resources["reservations"].(map[string]any)
		if reservations == nil {
			reservations = map[string]any{}
		}
		reservations["devices"] = []any{r.GPU}
		resources["reservations"] = reservations
		deploy["resources"] = resources
		service["deploy"] = deploy
	}
	if !r.Preview && len(r.Ports) > 0 {
		service["ports"] = r.Ports
	}
	if len(r.Volumes) > 0 {
		service["volumes"] = r.Volumes
	}
	if !r.Preview {
		custom := cloneMap(r.CustomOptions)
		ipv4, ipv6 := first(custom["ip"]), first(custom["ip6"])
		delete(custom, "ip")
		delete(custom, "ip6")
		if ipv4 != nil || ipv6 != nil {
			if !r.ConsistentName {
				network = map[string]any{}
				service["networks"] = map[string]any{r.Network: network}
			}
			if ipv4 != nil {
				network["ipv4_address"] = ipv4
			}
			if ipv6 != nil {
				network["ipv6_address"] = ipv6
			}
		}
		mergeRecursive(service, custom)
	}
	document := map[string]any{
		"services": map[string]any{r.ServiceName: service},
		"networks": map[string]any{r.Network: map[string]any{"external": true, "name": r.Network, "attachable": true}},
	}
	if len(r.NamedVolumes) > 0 {
		document["volumes"] = r.NamedVolumes
	}
	// Normalize json.Number before YAML encoding, retaining integer and string types.
	raw, err := json.Marshal(document)
	if err != nil || len(raw) > MaxDocumentBytes {
		return Result{}, ErrInvalid
	}
	var normalized any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&normalized); err != nil {
		return Result{}, ErrInvalid
	}
	normalized, err = normalizeNumbers(normalized, 0, new(int))
	if err != nil {
		return Result{}, ErrInvalid
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err = enc.Encode(normalized); err != nil {
		return Result{}, ErrInvalid
	}
	_ = enc.Close()
	if buf.Len() > MaxDocumentBytes {
		return Result{}, ErrInvalid
	}
	if err = ctx.Err(); err != nil {
		return Result{}, err
	}
	sum := sha256.Sum256(buf.Bytes())
	return Result{SchemaVersion: 1, Compose: buf.String(), SHA256: hex.EncodeToString(sum[:])}, nil
}
func cloneMap(m map[string]any) map[string]any {
	c := make(map[string]any, len(m))
	for k, v := range m {
		c[k] = cloneValue(v)
	}
	return c
}
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneMap(x)
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}
func normalizeNumbers(v any, depth int, count *int) (any, error) {
	*count++
	if depth > 32 || *count > 100000 {
		return nil, ErrInvalid
	}
	switch x := v.(type) {
	case json.Number:
		if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return n, nil
		}
		n, err := x.Float64()
		return n, err
	case map[string]any:
		for k, item := range x {
			n, err := normalizeNumbers(item, depth+1, count)
			if err != nil {
				return nil, err
			}
			x[k] = n
		}
	case []any:
		for i, item := range x {
			n, err := normalizeNumbers(item, depth+1, count)
			if err != nil {
				return nil, err
			}
			x[i] = n
		}
	}
	return v, nil
}
func first(v any) any {
	a, ok := v.([]any)
	if !ok || len(a) == 0 || a[0] == "" || a[0] == nil {
		return nil
	}
	return a[0]
}

// Match PHP array_merge_recursive: append lists, recursively merge maps, collect scalar collisions.
func mergeRecursive(dst, src map[string]any) {
	for k, v := range src {
		old, exists := dst[k]
		if !exists {
			dst[k] = v
			continue
		}
		a, aok := old.(map[string]any)
		b, bok := v.(map[string]any)
		if aok && bok {
			mergeRecursive(a, b)
			continue
		}
		list, ok := old.([]any)
		if !ok {
			if strings, yes := old.([]string); yes {
				for _, s := range strings {
					list = append(list, s)
				}
			} else {
				list = []any{old}
			}
		}
		if values, ok := v.([]any); ok {
			list = append(list, values...)
		} else {
			list = append(list, v)
		}
		dst[k] = list
	}
}
