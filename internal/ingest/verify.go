package ingest

import (
	"fmt"
	"io"
	"unsafe"

	"github.com/zeebo/blake3"
)

// VerifyMethod records which re-read strategy actually produced a Verify
// result, so a caller (and a test) can distinguish "cache-defeating
// unbuffered re-read" from "buffered floor" rather than treating every
// non-error Verify call as equally strong evidence. Decided once, at open
// time, per file.PickVerifyReader's doc comment -- never falls back
// mid-stream, since a half-cached read reported as cache-defeating would be
// worse than not claiming it at all.
type VerifyMethod string

const (
	// VerifyMethodUnbuffered means the re-read bypassed the OS page cache
	// (O_DIRECT on Linux; not yet implemented for other platforms -- see
	// verify_other.go).
	VerifyMethodUnbuffered VerifyMethod = "unbuffered"
	// VerifyMethodBufferedFloor means fsync+close+reopen was used without an
	// unbuffered/direct-I/O path -- the documented acceptable floor (issue
	// #2), used whenever the platform or filesystem doesn't support direct
	// I/O (a common case: O_DIRECT is refused with EINVAL on tmpfs, which a
	// CI runner's scratch directory often is).
	VerifyMethodBufferedFloor VerifyMethod = "buffered_floor"
)

// VerifyResult is one file's verification outcome.
type VerifyResult struct {
	Path     string
	Method   VerifyMethod
	Hash     string
	Verified bool // Hash == the wantHash Verify was called with
}

// Verify re-reads path (already fsync'd and closed by DualWrite) via
// openForVerify -- unbuffered/direct I/O where the platform and filesystem
// support it, the fsync+close+reopen floor otherwise -- recomputes
// BLAKE3-256 over the bytes actually read back, and reports whether it
// matches wantHash (the hash computed during the write). This is the "prove
// it, don't just claim it" half of the verified-write contract: a re-read
// straight from a warm page cache would hash the bytes the client just
// buffered, not the bytes that landed on the far side of an NFS/SMB mount,
// which is exactly the failure mode this function exists to rule out.
func Verify(path string, wantHash string, opts ...WriteOption) (VerifyResult, error) {
	r, method, err := openForVerify(path)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("ingest: open %s for verify: %w", path, err)
	}
	defer func() { _ = r.Close() }()

	h := blake3.New()
	var dst io.Writer = h
	if o := applyWriteOptions(opts); o.onBytes != nil {
		dst = io.MultiWriter(h, &progressWriter{onBytes: o.onBytes})
	}
	if _, err := copyAligned(dst, r); err != nil {
		return VerifyResult{}, fmt.Errorf("ingest: read %s for verify: %w", path, err)
	}
	got := fmt.Sprintf("%x", h.Sum(nil))

	return VerifyResult{
		Path:     path,
		Method:   method,
		Hash:     got,
		Verified: got == wantHash,
	}, nil
}

// directBufSize/directAlign size and align the read buffer used by both the
// O_DIRECT (verify_linux.go) and buffered-floor (verify_other.go, and
// Linux's own O_DIRECT-unsupported fallback) paths. 4MiB is a multiple of
// every logical block size in practical use (512B-4096B); alignment matters
// only for the O_DIRECT path, but using the same aligned buffer
// unconditionally keeps this one code path instead of two.
const (
	directBufSize = 4 * 1024 * 1024
	directAlign   = 4096
)

// alignedBuffer returns a size-byte slice whose backing array's start
// address is a multiple of align, by over-allocating and slicing -- the
// standard technique for satisfying O_DIRECT's buffer-alignment requirement
// in Go, which has no portable aligned-allocation primitive in the stdlib.
func alignedBuffer(size, align int) []byte {
	buf := make([]byte, size+align)
	addr := uintptr(unsafe.Pointer(&buf[0]))
	offset := 0
	if rem := addr % uintptr(align); rem != 0 {
		offset = align - int(rem)
	}
	return buf[offset : offset+size]
}

// copyAligned reads src into dst using a page-aligned, block-size-multiple
// buffer -- required for the O_DIRECT read path (verify_linux.go) and
// harmless for the buffered-floor path (verify_other.go, and Linux's own
// fallback when O_DIRECT isn't supported by the filesystem). Deliberately
// not plain io.Copy: io.Copy's internal buffer has no alignment guarantee,
// which would make an O_DIRECT-opened file's Read calls fail.
func copyAligned(dst io.Writer, src io.Reader) (int64, error) {
	buf := alignedBuffer(directBufSize, directAlign)
	var total int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err != nil {
			if err == io.EOF { //nolint:errorlint // io.Reader contract: compare exact sentinel, never wrapped
				return total, nil
			}
			return total, err
		}
	}
}
