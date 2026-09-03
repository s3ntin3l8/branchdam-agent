//go:build darwin

package ingest

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

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

	if _, err := unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1); err == nil {
		return f, VerifyMethodUnbuffered, nil
	} else if !errors.Is(err, unix.ENOTSUP) {
		f.Close()
		return nil, "", err
	}

	return f, VerifyMethodBufferedFloor, nil
}
