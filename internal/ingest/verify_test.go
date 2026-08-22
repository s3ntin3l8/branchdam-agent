package ingest

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"github.com/zeebo/blake3"
)

func TestAlignedBuffer(t *testing.T) {
	for _, align := range []int{512, 1024, 4096} {
		buf := alignedBuffer(1024*1024, align)
		if len(buf) != 1024*1024 {
			t.Errorf("len(buf) = %d, want %d", len(buf), 1024*1024)
		}
		addr := uintptr(unsafe.Pointer(&buf[0]))
		if addr%uintptr(align) != 0 {
			t.Errorf("address %v not aligned to %d", addr, align)
		}
	}
}

func TestVerifyMatchAndMismatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sample.bin")
	content := []byte("verification-test-payload-12345")
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}

	h := blake3.New()
	if _, err := h.Write(content); err != nil {
		t.Fatal(err)
	}
	wantHash := string(h.Sum(nil))
	wantHashHex := ""
	for _, b := range h.Sum(nil) {
		wantHashHex += string("0123456789abcdef"[b>>4]) + string("0123456789abcdef"[b&0xf])
	}
	_ = wantHash

	res, err := Verify(p, wantHashHex)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if !res.Verified {
		t.Errorf("expected Verified=true, got false (hash=%s, want=%s)", res.Hash, wantHashHex)
	}
	if res.Method != VerifyMethodUnbuffered && res.Method != VerifyMethodBufferedFloor {
		t.Errorf("unexpected method: %s", res.Method)
	}

	resBad, err := Verify(p, "bad-hash")
	if err != nil {
		t.Fatalf("Verify with bad hash error: %v", err)
	}
	if resBad.Verified {
		t.Error("expected Verified=false for mismatched hash")
	}
}

func TestVerifyNonexistentFile(t *testing.T) {
	_, err := Verify("/nonexistent/file.bin", "00")
	if err == nil {
		t.Error("expected error for nonexistent file")
	}
}
