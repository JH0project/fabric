package configuration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/JH0project/fabric/control/internal/environment"
)

type fakeEnvironment struct {
	value string
	calls int
	err   error
}

func (e *fakeEnvironment) Resolve(context.Context, environment.Reference) ([]environment.Variable, error) {
	e.calls++
	return []environment.Variable{{Key: "KEY", Value: e.value}}, e.err
}

type countingWriter struct{ calls int }

func (w *countingWriter) Write(_ context.Context, f File) (string, error) {
	w.calls++
	return fmt.Sprintf("%x", sha256.Sum256([]byte(*f.Content))), nil
}
func databaseTemplate() *TemplateSpec {
	return &TemplateSpec{Engine: "go-template/v1", Source: "{{range .env}}{{.key}}={{.value}}{{end}}", Data: map[string]any{"node": map[string]any{"id": "2"}}, Environment: &environment.Reference{ApplicationUUID: "app", ServerID: 2, TeamID: 1}}
}
func TestDatabaseEnvironmentPreflightAndDrift(t *testing.T) {
	w := &countingWriter{}
	s, _ := NewService(w, slog.New(slog.NewTextHandler(io.Discard, nil)))
	source := &fakeEnvironment{value: "old"}
	s.Environments = source
	r := Request{ServiceID: "app", Files: []File{{Path: "one", Template: databaseTemplate()}, {Path: "two", Template: databaseTemplate()}}}
	preflight := s.Render(context.Background(), r)
	if !preflight.Rendered || source.calls != 1 || w.calls != 0 {
		t.Fatal(preflight, source.calls, w.calls)
	}
	if _, exists := r.Files[0].Template.Data["env"]; exists {
		t.Fatal("caller request mutated")
	}
	for i := range r.Files {
		r.Files[i].ExpectedSHA256 = preflight.Files[i].SHA256
	}
	source.value = "new"
	got := s.Apply(context.Background(), r)
	if got.Applied || w.calls != 0 || got.Files[0].Code != "digest_mismatch" {
		t.Fatal(got, w.calls)
	}
	source.value = "old"
	if got := s.Apply(context.Background(), r); !got.Applied || w.calls != 2 {
		t.Fatal(got, w.calls)
	}
}
func TestDatabaseEnvironmentRejectsMixedOrWrongIdentityBeforeLookup(t *testing.T) {
	for _, change := range []func(*TemplateSpec){
		func(s *TemplateSpec) { s.Data["env"] = []any{} }, func(s *TemplateSpec) { s.Environment.ApplicationUUID = "other" },
		func(s *TemplateSpec) { s.Data["node"] = map[string]any{"id": "3"} }, func(s *TemplateSpec) { s.Environment.TeamID = -1 },
	} {
		w := &countingWriter{}
		s, _ := NewService(w, slog.New(slog.NewTextHandler(io.Discard, nil)))
		source := &fakeEnvironment{value: "secret"}
		s.Environments = source
		spec := databaseTemplate()
		change(spec)
		got := s.Apply(context.Background(), Request{ServiceID: "app", Files: []File{{Path: "one", Template: spec}}})
		if got.Applied || source.calls != 0 || w.calls != 0 || got.Files[0].Code != "environment_unavailable" {
			t.Fatal(got)
		}
	}
}
func TestMissingDatabaseEnvironmentNeverWritesLiteralSiblings(t *testing.T) {
	w := &countingWriter{}
	s, _ := NewService(w, slog.New(slog.NewTextHandler(io.Discard, nil)))
	text := "literal"
	got := s.Apply(context.Background(), Request{ServiceID: "app", Files: []File{{Path: "literal", Content: &text}, {Path: "template", Template: databaseTemplate()}}})
	if got.Applied || w.calls != 0 || strings.Contains(fmt.Sprint(got), "literal content") {
		t.Fatal(got)
	}
}
