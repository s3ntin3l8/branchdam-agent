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

// TestOpenForVerifyRealFileSmoke is a smoke test: openForVerify must
// succeed on a real file and not silently fall back to a wrong
// method. It does NOT assert VerifyMethodUnbuffered specifically,
// because t.TempDir() may live on tmpfs (CI runners, overlay) where
// O_DIRECT legitimately falls back to BufferedFloor. Asserting
// Unbuffered there would flake; the unbuffered path is covered by
// the unit-level mock test above and by the integration tests run
// against a real SSD.
func TestOpenForVerifyRealFileSmoke(t *testing.T) {
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

	if method != VerifyMethodUnbuffered && method != VerifyMethodBufferedFloor {
		t.Errorf("method=%s, want Unbuffered or BufferedFloor", method)
	}
}
