//go:build !linux

package ingest

import (
	"io"
	"os"
)

// openForVerify on non-Linux platforms uses the fsync+close+reopen floor
// only. macOS F_NOCACHE (fcntl) and Windows FILE_FLAG_NO_BUFFERING are
// deliberately not implemented in this PR -- no macOS or Windows host was
// available to validate an unbuffered implementation against (the same
// caveat the plan doc's UI-stack section flags for the tray shell's own
// darwin/windows packaging). This is the documented acceptable floor, not a
// silent gap: see VerifyMethodBufferedFloor's doc comment. A follow-up PR
// implementing either is additive and does not change this function's
// signature.
func openForVerify(path string) (io.ReadCloser, VerifyMethod, error) {
	f, err := os.Open(path) //nolint:gosec // path is our own just-written destination, not attacker input
	if err != nil {
		return nil, "", err
	}
	return f, VerifyMethodBufferedFloor, nil
}
