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

// openForVerify on macOS attempts to bypass the OS page cache via F_NOCACHE
// (fcntl). If fcntl succeeds, unbuffered direct I/O is achieved. If fcntl
// fails with ENOTSUP (unsupported filesystem), it falls back to the
// fsync+close+reopen floor (VerifyMethodBufferedFloor).  Any other fcntl
// error is returned to the caller so Verify can surface it instead of
// silently degrading.
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
