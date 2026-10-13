// Package configuration implements node-local, stateless file application.
package configuration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const MaxFileBytes = 5 * 1024 * 1024

var (
	ErrInvalid  = errors.New("invalid file specification")
	ErrConflict = errors.New("destination is not a regular file")
)

type File struct {
	Path    string  `json:"path"`
	Content *string `json:"content"`
	Mode    string  `json:"mode,omitempty"`
	UID     *int    `json:"uid,omitempty"`
	GID     *int    `json:"gid,omitempty"`
}
type root struct {
	path   string
	handle *os.Root
}
type pathLock struct {
	token chan struct{}
	users int
}
type Writer struct {
	roots []root
	mu    sync.Mutex
	locks map[string]*pathLock
}

func NewWriter(paths []string) (*Writer, error) {
	w := &Writer{locks: make(map[string]*pathLock)}
	for _, path := range paths {
		if !filepath.IsAbs(path) {
			w.Close()
			return nil, fmt.Errorf("%w: roots must be absolute", ErrInvalid)
		}
		path = filepath.Clean(path)
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			w.Close()
			return nil, fmt.Errorf("%w: root must be an existing directory", ErrInvalid)
		}
		handle, err := os.OpenRoot(path)
		if err != nil {
			w.Close()
			return nil, err
		}
		w.roots = append(w.roots, root{path, handle})
	}
	if len(w.roots) == 0 {
		return nil, fmt.Errorf("%w: at least one allowed root is required", ErrInvalid)
	}
	return w, nil
}
func (w *Writer) Close() error {
	var err error
	for _, r := range w.roots {
		err = errors.Join(err, r.handle.Close())
	}
	return err
}
func (w *Writer) resolve(path string) (*os.Root, string, error) {
	if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) || len(path) > 4096 {
		return nil, "", ErrInvalid
	}
	for _, part := range strings.FieldsFunc(path, func(c rune) bool { return c == '/' || c == '\\' }) {
		if part == ".." {
			return nil, "", ErrInvalid
		}
	}
	for _, r := range w.roots {
		rel, err := filepath.Rel(r.path, path)
		if err == nil && rel != "." && filepath.IsLocal(rel) {
			return r.handle, rel, nil
		}
	}
	return nil, "", ErrInvalid
}
func (w *Writer) acquire(ctx context.Context, path string) (func(), error) {
	w.mu.Lock()
	lock := w.locks[path]
	if lock == nil {
		lock = &pathLock{token: make(chan struct{}, 1)}
		lock.token <- struct{}{}
		w.locks[path] = lock
	}
	lock.users++
	w.mu.Unlock()
	releaseRef := func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		lock.users--
		if lock.users == 0 {
			delete(w.locks, path)
		}
	}
	select {
	case <-ctx.Done():
		releaseRef()
		return nil, ctx.Err()
	case <-lock.token:
		return func() { lock.token <- struct{}{}; releaseRef() }, nil
	}
}
func checkPath(r *os.Root, path string) (fs.FileInfo, error) {
	parts := strings.Split(path, string(filepath.Separator))
	var info fs.FileInfo
	for i := range parts {
		current := filepath.Join(parts[:i+1]...)
		var err error
		info, err = r.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, ErrConflict
		}
		if i < len(parts)-1 && !info.IsDir() {
			return nil, ErrConflict
		}
	}
	if !info.Mode().IsRegular() {
		return nil, ErrConflict
	}
	return info, nil
}
func (w *Writer) Write(ctx context.Context, spec File) (string, error) {
	if spec.Content == nil || len(*spec.Content) > MaxFileBytes {
		return "", ErrInvalid
	}
	mode := fs.FileMode(0644)
	if spec.Mode != "" {
		if len(spec.Mode) < 3 || len(spec.Mode) > 4 {
			return "", ErrInvalid
		}
		value, err := strconv.ParseUint(spec.Mode, 8, 32)
		if err != nil || value > 0777 {
			return "", ErrInvalid
		}
		mode = fs.FileMode(value)
	}
	if (spec.UID != nil && (*spec.UID < 0 || int64(*spec.UID) > 4294967294)) || (spec.GID != nil && (*spec.GID < 0 || int64(*spec.GID) > 4294967294)) {
		return "", ErrInvalid
	}
	r, rel, err := w.resolve(spec.Path)
	if err != nil {
		return "", err
	}
	unlock, err := w.acquire(ctx, filepath.Clean(spec.Path))
	if err != nil {
		return "", err
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	existing, err := checkPath(r, rel)
	if err != nil {
		return "", err
	}
	if existing != nil && spec.Mode == "" {
		mode = existing.Mode().Perm()
	}
	parent := filepath.Dir(rel)
	if err := r.MkdirAll(parent, 0755); err != nil {
		return "", err
	}
	// Recheck created parents. os.Root confines the actual I/O as well.
	if _, err := checkPath(r, rel); err != nil {
		return "", err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	temp := filepath.Join(parent, ".fabric-"+hex.EncodeToString(nonce[:]))
	f, err := r.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	defer r.Remove(temp)
	defer f.Close()
	reader := strings.NewReader(*spec.Content)
	buffer := make([]byte, 32*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, readErr := reader.Read(buffer)
		if n > 0 {
			written, err := f.Write(buffer[:n])
			if err != nil {
				return "", err
			}
			if written != n {
				return "", io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if spec.UID != nil || spec.GID != nil {
		uid, gid := -1, -1
		if spec.UID != nil {
			uid = *spec.UID
		}
		if spec.GID != nil {
			gid = *spec.GID
		}
		if err := f.Chown(uid, gid); err != nil {
			return "", err
		}
	}
	if err := f.Chmod(mode); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	expected := sha256.Sum256([]byte(*spec.Content))
	if digest != hex.EncodeToString(expected[:]) {
		return "", errors.New("file verification failed")
	}
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := checkPath(r, rel); err != nil {
		return "", err
	}
	if err := r.Rename(temp, rel); err != nil {
		return "", err
	}
	published, err := r.Lstat(rel)
	if err != nil {
		return "", err
	}
	if !os.SameFile(info, published) {
		return "", errors.New("published file verification failed")
	}
	return digest, nil
}
