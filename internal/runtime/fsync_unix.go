//go:build !windows && !darwin

// This file covers all non-darwin POSIX targets: Linux,
// FreeBSD, OpenBSD, NetBSD, DragonFly, illumos, Solaris. The
// syscall.Fsync approach is identical on all of them and matches
// the internal/runtime.go pathForGOOS table that lumps them
// under a single Linux+XDG branch.
package runtime

import (
	"fmt"
	"os"
	"syscall"
)

// fsyncDir opens dir and calls syscall.Fsync on the underlying
// file descriptor, then closes the file. An fsync(2) on a
// directory flushes the directory's inode and entry list to disk,
// which is the post-rename step that turns temp+rename into a
// durable atomicity guarantee across a power loss.
//
// Illumos/Solaris additionally need the file to be opened with
// O_RDONLY for dir fsync to work; os.Open defaults to O_RDONLY
// on a directory, so this is correct without an explicit flag.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory: %w", err)
	}
	defer func() { _ = d.Close() }()
	if err := syscall.Fsync(int(d.Fd())); err != nil {
		return fmt.Errorf("fsync: %w", err)
	}
	return nil
}
