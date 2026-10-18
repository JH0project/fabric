//go:build linux

package configuration

import (
	"io/fs"
	"syscall"
)

func existingOwnership(info fs.FileInfo) (int, int, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int(stat.Uid), int(stat.Gid), true
}
