// Package environment resolves application environment values without SQL or SDK types.
package environment

import (
	"context"
	"errors"
	"regexp"
	"strings"
)

var ErrUnavailable = errors.New("application environment unavailable")

// Reference identifies a Coolify application and one authorized deployment server.
// Coolify server IDs are not Fabric node/Ansible IDs.
type Reference struct {
	ApplicationUUID string `json:"application_uuid"`
	ServerID        int64  `json:"server_id"`
	TeamID          int64  `json:"team_id"`
}

func (r Reference) Valid() bool {
	return r.ApplicationUUID != "" && len(r.ApplicationUUID) <= 128 && r.ServerID > 0 && r.TeamID >= 0
}

type Variable struct{ Key, Value string }
type ApplicationVariable struct {
	Variable
	Runtime, Preview bool
}
type SharedVariable struct {
	Variable
	Scope   string
	ScopeID int64
}
type Snapshot struct {
	TeamID, ProjectID, EnvironmentID, ServerID int64
	Variables                                  []ApplicationVariable
	Shared                                     []SharedVariable
}

// Store returns one consistent, authorized snapshot. Decryption is an adapter concern.
type Store interface {
	Load(context.Context, Reference) (Snapshot, error)
}
type Resolver struct{ Store Store }

var sharedReference = regexp.MustCompile(`\{\{(.*?)\}\}`)

// Resolve matches Coolify's managed-template path, not shell/Compose escaping.
// Shared references are substituted once; missing/unknown references remain literal.
func (r Resolver) Resolve(ctx context.Context, ref Reference) ([]Variable, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ref.Valid() || r.Store == nil {
		return nil, ErrUnavailable
	}
	s, err := r.Store.Load(ctx, ref)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrUnavailable
	}
	if s.TeamID != ref.TeamID || s.ServerID != ref.ServerID || len(s.Variables)+len(s.Shared) > 8192 {
		return nil, ErrUnavailable
	}
	scopes := map[string]int64{"team": s.TeamID, "project": s.ProjectID, "environment": s.EnvironmentID, "server": s.ServerID}
	shared := map[string]string{}
	for _, v := range s.Shared {
		id, ok := scopes[v.Scope]
		if !ok || id != v.ScopeID {
			continue
		}
		key := v.Scope + "." + v.Key
		if _, exists := shared[key]; !exists {
			shared[key] = v.Value
		}
	}
	result := make([]Variable, 0, len(s.Variables))
	total := 0
	for _, v := range s.Variables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !v.Runtime || v.Preview || BuildOnly(v.Key) {
			continue
		}
		value := strings.Trim(v.Value, " \t\n\r\x00\v")
		for _, match := range sharedReference.FindAllStringSubmatch(value, -1) {
			if replacement, ok := shared[strings.Trim(match[1], " \t\n\r\x00\v")]; ok {
				if len(value)+len(replacement)*strings.Count(value, match[0]) > 5*1024*1024 {
					return nil, ErrUnavailable
				}
				value = strings.ReplaceAll(value, match[0], replacement)
			}
		}
		total += len(v.Key) + len(value)
		if total > 5*1024*1024 {
			return nil, ErrUnavailable
		}
		result = append(result, Variable{v.Key, value})
	}
	return result, nil
}

// BuildOnly identifies Coolify buildpack controls that are excluded at runtime.
func BuildOnly(key string) bool {
	switch key {
	case "NIXPACKS_PHP_ROOT_DIR", "NIXPACKS_PHP_FALLBACK_PATH", "NIXPACKS_SPA_OUTPUT_DIR", "RAILPACK_SKIP_MIGRATIONS":
		return false
	}
	return strings.HasPrefix(key, "NIXPACKS_") || strings.HasPrefix(key, "RAILPACK_")
}

// SharedReferences identifies values needed by this snapshot without resolving them.
func SharedReferences(vars []ApplicationVariable) map[string]bool {
	refs := map[string]bool{}
	for _, v := range vars {
		if !v.Runtime || v.Preview || BuildOnly(v.Key) {
			continue
		}
		for _, match := range sharedReference.FindAllStringSubmatch(v.Value, -1) {
			refs[strings.Trim(match[1], " \t\n\r\x00\v")] = true
		}
	}
	return refs
}
