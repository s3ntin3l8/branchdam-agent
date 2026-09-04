//go:build darwin

package runtime

import (
	"fmt"
	"os"
	"syscall"
)

// fsyncDir opens dir and calls fcntl(fd, F_FULLFSYNC) on the
// underlying file descriptor, then closes the file. F_FULLFSYNC is
// the macOS-only fcntl that asks the drive to flush its own cache,
// not just the OS cache -- a plain fsync(2) on macOS does NOT
// guarantee the rename reaches the disk, only the OS buffer cache,
// and a power loss between the two would still leave the dir
// pointing at the old inode. F_FULLFSYNC is the same fsync the
// macOS fsync(2) wrapper itself defers to in HFS+/APFS, and the
// same call internal/ingest's verify_darwin.go uses for the
// unbuffered-open floor's cache-defeating reopen.
//
// Note: F_FULLFSYNC on a *directory* file descriptor is not
// universally supported on HFS+ and is best-effort on APFS. APFS
// (the default on modern macOS) returns success; HFS+ returns
// ENOTSUP. Both are acceptable -- the function returns the error
// only for actual I/O failures, and the runtime.Save caller logs
// ENOTSUP at WARN level and continues, mirroring how the file-
// level chmod is best-effort on Windows.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	// F_FULLFSYNC = 51 on macOS. Hard-coded to avoid pulling
	// golang.org/x/sys into a repo whose other 3rd-party deps are
	// scoped to ingest + queue + tray; this is the only syscall
	// constant we'd need, and a const with a comment is cheaper
	// than a dependency.
	const F_FULLFSYNC = 51
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, d.Fd(), F_FULLFSYNC, 0); errno != 0 {
		return fmt.Errorf("fcntl F_FULLFSYNC: %w", errno)
	}
	return nil
}
