package environment

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type memoryStore struct {
	snapshot Snapshot
	err      error
}

func TestCoolifyRootServerAndTeamIdentity(t *testing.T) {
	ref := Reference{ApplicationUUID: "root-app", ServerID: 0, TeamID: 0}
	got, err := (Resolver{memoryStore{snapshot: Snapshot{Variables: []ApplicationVariable{{Variable: Variable{Key: "ROOT", Value: "value"}, Runtime: true}}}}}).Resolve(context.Background(), ref)
	if err != nil || len(got) != 1 || got[0].Value != "value" {
		t.Fatal("Coolify root identities rejected", err)
	}
	ref.ServerID = -1
	if ref.Valid() {
		t.Fatal("negative server identity accepted")
	}
}

func (s memoryStore) Load(context.Context, Reference) (Snapshot, error) { return s.snapshot, s.err }
func TestResolveCoolifyTemplateSemantics(t *testing.T) {
	s := Snapshot{TeamID: 10, ProjectID: 20, EnvironmentID: 30, ServerID: 40, Variables: []ApplicationVariable{
		{Variable: Variable{"SERVER", "  {{ server.NAME }}\n"}, Runtime: true},
		{Variable: Variable{"SCOPES", "{{team.NAME}}/{{project.NAME}}/{{environment.NAME}}"}, Runtime: true},
		{Variable: Variable{"MISSING", "{{server.MISSING}} {{unknown.NAME}}"}, Runtime: true},
		{Variable: Variable{"JSON", "{\"x\":\"$literal\"}"}, Runtime: true},
		{Variable: Variable{"ZERO", "0"}, Runtime: true},
		{Variable: Variable{"EMPTY", ""}, Runtime: true},
		{Variable: Variable{"BUILD", "hidden"}},
		{Variable: Variable{"PREVIEW", "hidden"}, Runtime: true, Preview: true},
		{Variable: Variable{"NIXPACKS_BUILD_CMD", "hidden"}, Runtime: true},
		{Variable: Variable{"RAILPACK_BUILD_CMD", "hidden"}, Runtime: true},
		{Variable: Variable{"NIXPACKS_PHP_ROOT_DIR", "/app/public"}, Runtime: true},
	}, Shared: []SharedVariable{
		{Variable: Variable{"NAME", "wrong-server"}, Scope: "server", ScopeID: 41},
		{Variable: Variable{"NAME", "node"}, Scope: "server", ScopeID: 40},
		{Variable: Variable{"NAME", "team"}, Scope: "team", ScopeID: 10},
		{Variable: Variable{"NAME", "project"}, Scope: "project", ScopeID: 20},
		{Variable: Variable{"NAME", "environment"}, Scope: "environment", ScopeID: 30},
	}}
	got, err := (Resolver{memoryStore{snapshot: s}}).Resolve(context.Background(), Reference{"app", 40, 10})
	want := []Variable{{"SERVER", "node"}, {"SCOPES", "team/project/environment"}, {"MISSING", "{{server.MISSING}} {{unknown.NAME}}"}, {"JSON", "{\"x\":\"$literal\"}"}, {"ZERO", "0"}, {"EMPTY", ""}, {"NIXPACKS_PHP_ROOT_DIR", "/app/public"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("resolution differs", got, err)
	}
}
func TestResolverDoesNotExposeStoreErrorsOrCrossScopeData(t *testing.T) {
	for _, s := range []memoryStore{{err: errors.New("secret database value")}, {snapshot: Snapshot{TeamID: 11, ServerID: 40}}, {snapshot: Snapshot{TeamID: 10, ServerID: 41}}} {
		got, err := (Resolver{s}).Resolve(context.Background(), Reference{"app", 40, 10})
		if len(got) != 0 || !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "secret") {
			t.Fatal("unsafe failure", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Resolver{memoryStore{snapshot: Snapshot{TeamID: 10, ServerID: 40, Variables: []ApplicationVariable{{Runtime: true}}}}}).Resolve(ctx, Reference{"app", 40, 10})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestSharedValuesAreNotRecursivelyExpandedOrTrimmed(t *testing.T) {
	s := Snapshot{TeamID: 1, ServerID: 2, Variables: []ApplicationVariable{{Variable: Variable{"KEY", "{{server.VALUE}}"}, Runtime: true}}, Shared: []SharedVariable{{Variable: Variable{"VALUE", " {{team.OTHER}} \n"}, Scope: "server", ScopeID: 2}}}
	got, err := (Resolver{memoryStore{snapshot: s}}).Resolve(context.Background(), Reference{"app", 2, 1})
	if err != nil || got[0].Value != " {{team.OTHER}} \n" {
		t.Fatal(got, err)
	}
}
