//go:build darwin

package ingest

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// openForVerify on macOS attempts to bypass the OS page cache via F_NOCACHE
// (fcntl). If fcntl succeeds, unbuffered direct I/O is achieved. If fcntl
// fails (e.g. on unsupported filesystems), it falls back to the
// fsync+close+reopen floor (VerifyMethodBufferedFloor).
func openForVerify(path string) (io.ReadCloser, VerifyMethod, error) {
	f, err := os.Open(path) //nolint:gosec // path is our own just-written destination, not attacker input
	if err != nil {
		return nil, "", err
	}

	if _, err := unix.FcntlInt(f.Fd(), unix.F_NOCACHE, 1); err == nil {
		return f, VerifyMethodUnbuffered, nil
	}

	return f, VerifyMethodBufferedFloor, nil
}
