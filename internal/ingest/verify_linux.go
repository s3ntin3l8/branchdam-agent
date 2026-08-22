//go:build linux

package ingest

import (
	"io"
	"os"
	"syscall"
)

// openForVerify tries an O_DIRECT (unbuffered) open first -- the preferred
// cache-defeating path per issue #2. A common, expected failure is EINVAL:
// O_DIRECT is refused outright by tmpfs and some overlay/network
// filesystems, which is exactly the kind of scratch directory a CI runner
// or sandboxed dev environment is likely to use. That failure (and only
// that failure path, decided once here before any byte is read -- never a
// mid-stream fallback, see VerifyMethod's doc comment) falls back to the
// documented acceptable floor: a plain reopen of a file DualWrite already
// fsync'd and closed.
func openForVerify(path string) (io.ReadCloser, VerifyMethod, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECT, 0)
	if err == nil {
		return f, VerifyMethodUnbuffered, nil
	}

	f2, err2 := os.Open(path) //nolint:gosec // path is our own just-written destination, not attacker input
	if err2 != nil {
		return nil, "", err2
	}
	return f2, VerifyMethodBufferedFloor, nil
}
