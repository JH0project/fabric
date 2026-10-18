//go:build linux

package configuration

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRenderedFilePreservesOwnerAndExplicitOverrides(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to simulate a service-owned file")
	}
	root := t.TempDir()
	path := filepath.Join(root, "config")
	if err := os.WriteFile(path, []byte("old"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, 9999, 0); err != nil {
		t.Fatal(err)
	}
	writer, err := NewWriter([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	service, err := NewService(writer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	template := &TemplateSpec{Engine: "go-template/v1", Source: "{{.value}}", Data: map[string]any{"value": "new"}}
	for _, tc := range []struct {
		uid, gid         *int
		wantUID, wantGID int
	}{
		{nil, nil, 9999, 0},
		{ptrInt(123), ptrInt(456), 123, 456},
	} {
		result := service.Apply(context.Background(), Request{ServiceID: "owner-test", Files: []File{{Path: path, Template: template, UID: tc.uid, GID: tc.gid}}})
		if !result.Applied {
			t.Fatal(result)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		stat := info.Sys().(*syscall.Stat_t)
		if int(stat.Uid) != tc.wantUID || int(stat.Gid) != tc.wantGID || info.Mode().Perm() != 0700 {
			t.Fatal(stat.Uid, stat.Gid, info.Mode())
		}
	}
	// Literal clients keep their existing ownership semantics.
	result := service.Apply(context.Background(), Request{ServiceID: "owner-test", Files: []File{{Path: path, Content: ptr("literal")}}})
	if !result.Applied {
		t.Fatal(result)
	}
	info, _ := os.Stat(path)
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		t.Fatal("literal behavior changed")
	}
}
func ptrInt(v int) *int { return &v }
