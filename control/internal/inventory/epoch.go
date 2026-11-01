package inventory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
)

type Epoch struct {
	Number uint64
	lock   *flock.Flock
}

// AcquireEpoch holds an OS lock for the entire publisher lifetime. The directory
// must be persistent across container recreation and exclusive to one node UUID.
func AcquireEpoch(dir, node string) (*Epoch, error) {
	id, parseErr := uuid.Parse(node)
	if parseErr != nil || id == uuid.Nil || id.String() != node {
		return nil, errors.New("canonical node UUID required for epoch file")
	}
	if dir == "" {
		return nil, errors.New("persistent inventory state directory is required")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, node+".epoch")
	lock := flock.New(path + ".lock")
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("inventory publisher already owns this node state directory")
	}
	e := &Epoch{lock: lock}
	success := false
	defer func() {
		if !success {
			_ = lock.Close()
		}
	}()
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		e.Number, err = strconv.ParseUint(strings.TrimSpace(string(data)), 10, 63)
		if err != nil || e.Number == 0 {
			return nil, errors.New("invalid persisted inventory epoch; refusing reset")
		}
	}
	if e.Number >= 1<<63-1 {
		return nil, errors.New("inventory epoch exhausted")
	}
	e.Number++
	f, err := os.CreateTemp(dir, ".epoch-*")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, writeErr := fmt.Fprintf(f, "%d\n", e.Number)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, path); err != nil {
		return nil, err
	}
	// Sync the directory where supported; Windows cannot fsync directories.
	if d, openErr := os.Open(dir); openErr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	success = true
	return e, nil
}
func (e *Epoch) Close() error { return e.lock.Close() }
