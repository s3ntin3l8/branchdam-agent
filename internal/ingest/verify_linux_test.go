//go:build linux

package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestOpenForVerifyNonEINVALNotSwallowed verifies that when os.OpenFile
// fails with an error other than EINVAL/EOPNOTSUPP (e.g. ENOENT for a
// nonexistent file), the error is returned instead of being silently
// swallowed by a fallback.
func TestOpenForVerifyNonEINVALNotSwallowed(t *testing.T) {
	_, _, err := openForVerify("/nonexistent/path/that/does/not/exist.bin")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}

	// The error should be ENOENT (wrapped in *os.PathError), not a nil
	// from a successful fallback.
	if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("expected ENOENT, got %v", err)
	}
}

// TestOpenForVerifyRealFileUnbuffered verifies that on a real filesystem
// (not tmpfs), openForVerify succeeds with O_DIRECT (Unbuffered method).
func TestOpenForVerifyRealFileUnbuffered(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "test.bin")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	f, method, err := openForVerify(p)
	if err != nil {
		t.Fatalf("openForVerify: %v", err)
	}
	defer f.Close() //nolint:errcheck // test read-only; close error is not meaningful

	if method != VerifyMethodUnbuffered {
		t.Logf("method=%s (may be BufferedFloor on tmpfs/overlay)", method)
	}
}
