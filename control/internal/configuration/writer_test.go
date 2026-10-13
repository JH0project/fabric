package configuration

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func ptr(s string) *string { return &s }
func testWriter(t *testing.T) (*Writer, string) {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWriter([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Close() })
	return w, dir
}
func TestWriteReplaceAndEmptyContent(t *testing.T) {
	w, dir := testWriter(t)
	path := filepath.Join(dir, "nested", "config.json")
	for _, content := range []string{"initial", "", "0", "replacement\n"} {
		digest, err := w.Write(context.Background(), File{Path: path, Content: ptr(content)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != content {
			t.Fatalf("content %q: %q, %v", content, got, err)
		}
		expected := sha256.Sum256([]byte(content))
		if digest != hex.EncodeToString(expected[:]) {
			t.Fatal("incorrect digest")
		}
	}
}
func TestRejectInvalidPathsAndDirectories(t *testing.T) {
	w, dir := testWriter(t)
	directory := filepath.Join(dir, "existing")
	if err := os.Mkdir(directory, 0755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(directory, "keep")
	os.WriteFile(sentinel, []byte("keep"), 0600)
	for _, path := range []string{dir, "relative/file", filepath.Join(t.TempDir(), "outside"), dir + string(filepath.Separator) + ".." + string(filepath.Separator) + "outside"} {
		if _, err := w.Write(context.Background(), File{Path: path, Content: ptr("bad")}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	if _, err := w.Write(context.Background(), File{Path: directory, Content: ptr("bad")}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(sentinel); string(data) != "keep" {
		t.Fatal("directory contents changed")
	}
}
func TestRejectSymlinks(t *testing.T) {
	w, dir := testWriter(t)
	external := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(external, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := w.Write(context.Background(), File{Path: filepath.Join(link, "file"), Content: ptr("bad")}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	original := filepath.Join(dir, "original")
	os.WriteFile(original, []byte("keep"), 0600)
	fileLink := filepath.Join(dir, "file-link")
	if err := os.Symlink(original, fileLink); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(context.Background(), File{Path: fileLink, Content: ptr("bad")}); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(original); string(data) != "keep" {
		t.Fatal("symlink target overwritten")
	}
}
func TestInvalidContentModeAndCancellation(t *testing.T) {
	w, dir := testWriter(t)
	path := filepath.Join(dir, "file")
	for _, spec := range []File{
		{Path: path},
		{Path: path, Content: ptr(strings.Repeat("x", MaxFileBytes+1))},
		{Path: path, Content: ptr("x"), Mode: "7777"},
		{Path: path, Content: ptr("x"), Mode: "08"},
	} {
		if _, err := w.Write(context.Background(), spec); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	unlock, err := w.acquire(context.Background(), filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := w.Write(ctx, File{Path: path, Content: ptr("x")}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	unlock()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled request wrote a file")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.locks) != 0 {
		t.Fatal("path locks leaked")
	}
}
func TestConcurrentWritesAreComplete(t *testing.T) {
	w, dir := testWriter(t)
	path := filepath.Join(dir, "file")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		content := strings.Repeat(string(rune('a'+i)), 64*1024)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.Write(context.Background(), File{Path: path, Content: ptr(content)}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 64*1024 {
		t.Fatal("incomplete write", err)
	}
	for _, b := range data {
		if b != data[0] {
			t.Fatal("interleaved content")
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary files leaked")
	}
}
