// Package hashing is a byte-for-byte port of two functions from branchDAM's
// own internal/hashing/hashing.go (commit c570690, lines 26-58 and 66-86 for
// FastHash/sampleRegions, 101-112 for PerceptualHash) -- nothing under
// branchdam's internal/ is importable cross-module, so this is a copy, not
// an import. Ported deliberately narrowly: only FastHash and PerceptualHash,
// per the plan's conformance contract. FullHash (BLAKE3-256 over the whole
// stream) and HammingDistance are NOT ported here -- out of scope for M0,
// which only needs the three pieces of server logic the plan calls out
// (FastHash, PerceptualHash's call sequence, naming.Stem).
//
// PerceptualHash is a thin wrapper over goimagehash.PerceptionHash from the
// same public library (github.com/corona10/goimagehash) branchDAM itself
// depends on -- this makes it exact by construction, not a reimplementation
// that could disagree on resize filter or median convention (see the plan's
// decision 2: that disagreement is exactly why this whole repo is Go, not
// Rust).
//
// FastHash's golden vectors in hashing_test.go are transcribed verbatim from
// branchDAM's own committed internal/hashing/hashing_test.go
// (TestFastHashGoldenVectors) -- those vectors were themselves produced by
// running branchDAM's actual FastHash implementation, so reusing them is
// running-the-implementation once removed. TestFastHashAgainstBranchdamHarness's
// doc comment records the additional throwaway-harness run performed to
// reconfirm them (see internal/hashing/hashing_test.go).
package hashing

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"

	"github.com/cespare/xxhash/v2"
	"github.com/corona10/goimagehash"
)

// fastHashSampleSize is 2MiB per region, matching branchDAM's spec Pillar 1.
const fastHashSampleSize = 2 * 1024 * 1024

// FastHash computes a cheap xxHash64 fingerprint over the first, middle, and
// last 2MiB of the file plus its total size, in that order. For files
// smaller than 6MiB the three regions overlap (even coincide entirely for a
// file under 2MiB) -- overlap is fine and deliberate, ported verbatim from
// branchDAM's own sampleRegions. This is NOT a substitute for a full-file
// digest: a naive whole-file hash produces a different, wrong value here.
// Returns 16 lowercase hex characters (64 bits).
func FastHash(r io.ReaderAt, size int64) (string, error) {
	h := xxhash.New()
	buf := make([]byte, fastHashSampleSize)

	for _, reg := range sampleRegions(size) {
		n := reg.end - reg.start
		if n == 0 {
			continue
		}
		read, err := r.ReadAt(buf[:n], reg.start)
		if err != nil && (!errors.Is(err, io.EOF) || int64(read) != n) {
			return "", fmt.Errorf("fast hash: read region [%d,%d): %w", reg.start, reg.end, err)
		}
		if _, err := h.Write(buf[:read]); err != nil {
			return "", fmt.Errorf("fast hash: %w", err)
		}
	}

	var sizeBuf [8]byte
	binary.BigEndian.PutUint64(sizeBuf[:], uint64(size))
	if _, err := h.Write(sizeBuf[:]); err != nil {
		return "", fmt.Errorf("fast hash: %w", err)
	}

	return fmt.Sprintf("%016x", h.Sum64()), nil
}

type region struct{ start, end int64 }

// sampleRegions returns the (first, middle, last) 2MiB windows for size,
// clamped to [0, size] so they overlap rather than go out of bounds for
// small files. Ported verbatim, including the size<=0 early return and the
// clamp closure -- de-duping or skipping the overlapping regions for a
// sub-2MiB file is the single most likely silent port bug (a naive port
// might try to "optimize" the redundant reads).
func sampleRegions(size int64) [3]region {
	if size <= 0 {
		return [3]region{{0, 0}, {0, 0}, {0, 0}}
	}
	clamp := func(v int64) int64 {
		if v < 0 {
			return 0
		}
		if v > size {
			return size
		}
		return v
	}

	first := region{0, clamp(fastHashSampleSize)}
	middleStart := clamp(size/2 - fastHashSampleSize/2)
	middle := region{middleStart, clamp(middleStart + fastHashSampleSize)}
	lastStart := clamp(size - fastHashSampleSize)
	last := region{lastStart, size}
	return [3]region{first, middle, last}
}

// PerceptualHash computes a 64-bit DCT perceptual hash of img. Takes an
// already-decoded image.Image, same as branchDAM's own PerceptualHash --
// decoding format selection belongs to the caller (see internal/phash for
// this repo's port of branchDAM's probe.ExtractPHash decode-then-fallback
// call sequence).
func PerceptualHash(img image.Image) (int64, error) {
	h, err := goimagehash.PerceptionHash(img)
	if err != nil {
		return 0, fmt.Errorf("perceptual hash: %w", err)
	}
	return int64(h.GetHash()), nil
}

// StreamingFastHasher is an io.Writer that, fed a file's bytes in a single
// forward pass (offset 0 through size, in order, any chunk size), captures
// exactly the bytes FastHash(io.ReaderAt, size) would read via
// sampleRegions -- so a caller that must stream a source exactly once (the
// M1 dual-copy card writer: one read of the card, two writes, no second
// pass) can still obtain byte-identical parity with FastHash's ReaderAt-based
// algorithm, never a reimplementation or approximation of it.
//
// This is NOT a general-purpose streaming hasher: Write must be called with
// monotonically increasing, contiguous, non-overlapping byte ranges
// covering [0, size) exactly once (i.e. a plain sequential copy loop) for
// Sum to agree with FastHash. It exists specifically to sit inside an
// io.MultiWriter alongside the destination file writers during that single
// pass.
type StreamingFastHasher struct {
	size    int64
	regions [3]region
	bufs    [3][]byte
	off     int64
}

// NewStreamingFastHasher allocates capture buffers for size's three
// sample windows (sampleRegions(size) -- identical clamping/overlap
// behavior as FastHash itself, including the size<=0 and sub-6MiB
// overlapping-window cases). Total buffer size is at most 6MiB regardless
// of how large size is.
func NewStreamingFastHasher(size int64) *StreamingFastHasher {
	regions := sampleRegions(size)
	h := &StreamingFastHasher{size: size, regions: regions}
	for i, r := range regions {
		n := r.end - r.start
		if n < 0 {
			n = 0
		}
		h.bufs[i] = make([]byte, n)
	}
	return h
}

// Write implements io.Writer, copying p's bytes into whichever of the three
// capture buffers (possibly more than one, for overlapping windows on a
// sub-6MiB file) they fall into, tracked against the running offset since
// the hasher was constructed. Always returns (len(p), nil) -- there is
// nothing in this operation that can fail.
func (h *StreamingFastHasher) Write(p []byte) (int, error) {
	n := len(p)
	chunkStart := h.off
	chunkEnd := h.off + int64(n)
	for i, r := range h.regions {
		if r.end <= r.start {
			continue
		}
		lo := max(chunkStart, r.start)
		hi := min(chunkEnd, r.end)
		if lo < hi {
			copy(h.bufs[i][lo-r.start:hi-r.start], p[lo-chunkStart:hi-chunkStart])
		}
	}
	h.off += int64(n)
	return n, nil
}

// Sum computes the FastHash-equivalent digest from the captured windows:
// xxHash64 over first, then middle, then last window's bytes (in that
// order, matching FastHash's ReadAt call order), then the raw 8-byte
// big-endian size appended to the hash state, formatted as 16 lowercase hex
// characters. Byte-identical to FastHash(sourceReaderAt, size) provided
// Write was called as documented on StreamingFastHasher.
func (h *StreamingFastHasher) Sum() string {
	hh := xxhash.New()
	for _, buf := range h.bufs {
		_, _ = hh.Write(buf)
	}
	var sizeBuf [8]byte
	binary.BigEndian.PutUint64(sizeBuf[:], uint64(h.size))
	_, _ = hh.Write(sizeBuf[:])
	return fmt.Sprintf("%016x", hh.Sum64())
}
