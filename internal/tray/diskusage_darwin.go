//go:build darwin

package tray

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// diskUsage returns free/total bytes for the volume containing path, via
// statfs(2). Unlike Linux's Statfs_t, darwin's has no separate Frsize --
// Bsize is already the actual allocation unit Blocks/Bavail are counted
// in (see diskusage_linux.go's own doc comment for why Linux needs the
// distinction and darwin doesn't).
func diskUsage(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	blockSize := uint64(st.Bsize)
	return st.Bavail * blockSize, st.Blocks * blockSize, nil
}
