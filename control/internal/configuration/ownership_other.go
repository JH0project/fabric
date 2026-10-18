//go:build !linux

package configuration

import "io/fs"

func existingOwnership(fs.FileInfo) (int, int, bool) { return 0, 0, false }
