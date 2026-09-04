//go:build darwin

package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// TestOpenForVerifyFcntlENOTSUPTakesBufferedFloor pins issue #160 /
// #102's darwin half: when fcntl(F_NOCACHE) fails with ENOTSUP
// (the documented "unsupported filesystem" signal), openForVerify
// must fall back to VerifyMethodBufferedFloor -- not propagate the
// error to the caller.
//
// Injecting the fcntl error is the only portable way to test this
// path on CI: macOS's own APFS accepts F_NOCACHE on a tmpfs-backed
// t.TempDir() in practice, so a real-path test wouldn't reliably
// reach the ENOTSUP branch (a real filesystem that rejects
// F_NOCACHE in CI is exactly the kind of thing this PR's
// indirection-based test approach is meant to avoid -- see the
// Linux twin TestOpenForVerifyNonEINVALNotSwallowed's note on
// tmpfs).
func TestOpenForVerifyFcntlENOTSUPTakesBufferedFloor(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := fcntlFncacheFn
	t.Cleanup(func() { fcntlFncacheFn = orig })
	fcntlFncacheFn = func(_ uintptr) error { return unix.ENOTSUP }

	f, method, err := openForVerify(p)
	if err != nil {
		t.Fatalf("openForVerify should fall back on ENOTSUP, got err: %v", err)
	}
	defer f.Close() //nolint:errcheck // test read-only; close error is not meaningful

	if method != VerifyMethodBufferedFloor {
		t.Errorf("method = %s, want BufferedFloor on ENOTSUP", method)
	}
}

// TestOpenForVerifyFcntlNonENOTSUPPropagates pins the second half
// of the darwin fix: any fcntl error that ISN'T ENOTSUP (e.g.
// EINVAL, EBADF after a real OS fault) must NOT be silently
// swallowed by the buffered-floor fallback. openForVerify must
// close the file it opened and return the error so Verify can
// surface it instead of silently degrading to a wrong method.
//
// Mirrors the Linux twin's
// TestOpenForVerifyNonEINVALNotSwallowed shape (non-supported error
// propagates rather than silently swallowed by fallback).
func TestOpenForVerifyFcntlNonENOTSUPPropagates(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := fcntlFncacheFn
	t.Cleanup(func() { fcntlFncacheFn = orig })
	// EIO is a plausible "real" fcntl failure -- not ENOTSUP, so
	// the propagation branch is the one under test.
	fcntlFncacheFn = func(_ uintptr) error { return syscall.EIO }

	f, method, err := openForVerify(p)
	if err == nil {
		_ = f.Close() //nolint:errcheck // test path; close error not meaningful
		t.Fatal("openForVerify should propagate non-ENOTSUP fcntl errors, got nil")
	}
	if f != nil {
		t.Errorf("openForVerify returned non-nil ReadCloser alongside an error: %v", f)
	}
	if method != "" {
		t.Errorf("method = %q, want \"\" on propagated error", method)
	}
	if !errors.Is(err, syscall.EIO) {
		t.Errorf("propagated err = %v, want EIO", err)
	}
}
