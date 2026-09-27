//go:build linux

package tray

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// diskUsage returns free/total bytes for the volume containing path, via
// statfs(2). Blocks/Bavail are counted in units of Frsize (the fragment
// size statvfs(3) documents as the actual allocation unit), not Bsize (the
// "preferred" I/O transfer size) -- the two are equal on most local
// filesystems but can differ, and Frsize is the one verified against `df
// -B1` in diskusage_linux_test.go. A zero Frsize (seen on some
// pseudo-filesystems that don't report one) falls back to Bsize rather
// than dividing by zero.
func diskUsage(path string) (free, total uint64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	blockSize := uint64(st.Frsize)
	if blockSize == 0 {
		blockSize = uint64(st.Bsize)
	}
	return st.Bavail * blockSize, st.Blocks * blockSize, nil
}
