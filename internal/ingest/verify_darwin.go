//go:build darwin

package ingest

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// fcntlFncacheFn is the indirection used to call fcntl(F_NOCACHE) on a
// file descriptor. Indirected through a package var so tests can
// substitute an error-returning stub (mirrors writer.go's
// syncParentDirFn and ingest.go's cHtimesFn patterns) without having to
// reach a real filesystem that legitimately rejects F_NOCACHE -- a
// property macOS's own APFS doesn't reliably exhibit, even on a tmpfs-
// backed t.TempDir().
var fcntlFncacheFn = func(fd uintptr) error {
	_, err := unix.FcntlInt(fd, unix.F_NOCACHE, 1)
	return err
}

// openForVerify on macOS asks the kernel not to cache the re-read via
// fcntl(F_NOCACHE). If fcntl fails with ENOTSUP (unsupported filesystem),
// it falls back to the fsync+close+reopen floor (VerifyMethodBufferedFloor).
// Any other fcntl error is returned to the caller so Verify can surface it
// instead of silently degrading.
//
// F_NOCACHE is weaker than Linux's O_DIRECT or Windows'
// FILE_FLAG_NO_BUFFERING, and VerifyMethodUnbuffered overstates it on
// macOS: it is advisory, not a guaranteed cache bypass. It turns off
// caching for data this descriptor reads from now on, but pages that are
// ALREADY resident in the unified buffer cache -- typically every page
// DualWrite just wrote through its own descriptor -- may still be served
// from memory, and the file system is free to ignore the hint (SMB and
// other network file systems decide for themselves). A successful
// F_NOCACHE verify on macOS is therefore at least the documented buffered
// floor (fsync, close, reopen) and often better, but cannot prove the
// bytes came back from the device. requireUnbuffered treats it as
// unbuffered, so it does not close that gap on macOS. See
// docs/platform-support.md ("Verify cache bypass per platform").
func openForVerify(path string) (io.ReadCloser, VerifyMethod, error) {
	f, err := os.Open(path) //nolint:gosec // path is our own just-written destination, not attacker input
	if err != nil {
		return nil, "", err
	}

	if err := fcntlFncacheFn(f.Fd()); err == nil {
		return f, VerifyMethodUnbuffered, nil
	} else if !errors.Is(err, unix.ENOTSUP) {
		f.Close()
		return nil, "", err
	}

	return f, VerifyMethodBufferedFloor, nil
}
