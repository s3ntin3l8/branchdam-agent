//go:build !linux && !darwin && !windows

package ingest

import (
	"io"
	"os"
)

// openForVerify on other platforms uses the fsync+close+reopen floor only.
// Linux uses O_DIRECT, macOS uses F_NOCACHE, and Windows uses
// FILE_FLAG_NO_BUFFERING.
func openForVerify(path string) (io.ReadCloser, VerifyMethod, error) {
	f, err := os.Open(path) //nolint:gosec // path is our own just-written destination, not attacker input
	if err != nil {
		return nil, "", err
	}
	return f, VerifyMethodBufferedFloor, nil
}
