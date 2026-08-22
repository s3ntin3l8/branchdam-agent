package hashing

import (
	"bytes"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"testing"
)

func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestFastHashGoldenVectors's cases are transcribed verbatim from
// branchDAM's own committed internal/hashing/hashing_test.go
// (TestFastHashGoldenVectors, commit c570690) -- those vectors were
// produced by running branchDAM's actual FastHash implementation, and were
// reconfirmed passing (`go test ./internal/hashing/...`) in a checkout of
// that commit immediately before transcribing them here. This is the same
// bespoke sampling scheme (first/middle/last 2MiB + big-endian size
// suffix), so a port that de-dupes the overlapping regions for a sub-2MiB
// file, or reads the wrong window, produces a plausible-looking but wrong
// 16-hex-char value that would fail every one of these cases.
func TestFastHashGoldenVectors(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"empty", nil, "34c96acdcadb1bbb"},
		{"tiny_13b", []byte("hello, world!"), "a17463135ce85764"},
		{"exactly_2mib", bytes.Repeat([]byte{0xAB}, 2*1024*1024), "c519d4df8b74ba0d"},
		{"3mib_pattern", patternBytes(3 * 1024 * 1024), "2c49f596a9f6d43e"},
		{"7mib_pattern", patternBytes(7 * 1024 * 1024), "18da4c4b0be74ade"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := FastHash(bytes.NewReader(c.data), int64(len(c.data)))
			if err != nil {
				t.Fatalf("FastHash: %v", err)
			}
			if got != c.want {
				t.Errorf("FastHash(%s) = %q, want %q", c.name, got, c.want)
			}
			if len(got) != 16 {
				t.Errorf("len(FastHash(%s)) = %d, want 16", c.name, len(got))
			}
		})
	}
}

// TestFastHashDeterministicAndSensitive mirrors branchDAM's own test of the
// same name: same bytes -> same hash, and a change anywhere in the sampled
// regions (including just the trailing byte, which only the "last" region
// and the size-suffix could catch) -> a different hash.
func TestFastHashDeterministicAndSensitive(t *testing.T) {
	a := patternBytes(5 * 1024 * 1024)
	b := make([]byte, len(a))
	copy(b, a)

	h1, err := FastHash(bytes.NewReader(a), int64(len(a)))
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	h2, err := FastHash(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("identical content hashed differently: %q vs %q", h1, h2)
	}

	b[len(b)-1] ^= 0xFF
	h3, err := FastHash(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	if h1 == h3 {
		t.Error("flipping the last byte did not change FastHash -- the 'last 2MiB' region isn't being sampled")
	}
}

// TestPerceptualHashGoldenVectors decodes the three committed image fixtures
// (testdata/gradient.{png,jpg,gif}) -- a 64x64 deterministic RGB gradient,
// deliberately not a solid color, so goimagehash's DCT actually has
// variation to hash -- and compares PerceptualHash's output against values
// produced by running branchDAM's own hashing.PerceptualHash on the
// byte-identical fixture files.
//
// How the vectors and fixtures were generated: a throwaway
// cmd/agentgoldenharness_throwaway/main.go was added to a checkout of the
// branchdam repo (commit c570690), importing its internal/hashing directly
// (this is the only way to reach code under branchdam's internal/ -- it is
// not importable cross-module). It built a 64x64 RGB gradient image via
// Go's stdlib color.RGBA (R/G/B derived linearly from x/y position),
// encoded it to PNG/JPEG(q=90)/GIF using the stdlib encoders, wrote those
// three files out, then decoded each back (image.Decode, after blank-
// importing only image/gif, image/jpeg, image/png -- the same registered
// decoder set probe.go uses) and ran branchdam's hashing.PerceptualHash on
// each decoded image.Image, printing the resulting int64. The PNG/JPEG/GIF
// bytes it wrote are copied verbatim into this package's testdata/; the
// int64 values it printed are the "want" column below. The harness was
// deleted from the branchdam tree immediately after (never committed
// there). Because PerceptualHash is a one-line wrapper over
// goimagehash.PerceptionHash -- the identical public dependency this
// package also imports -- this is exact by construction, not a
// reimplementation: the only way this test could fail is a decode-path
// difference (wrong registered format set, or a goimagehash version skew),
// which is exactly what it's designed to catch.
//
// Note PNG (lossless) reproduces the exact same hash as hashing the raw
// image.Image directly, while JPEG (lossy) and GIF (256-color palette
// quantization) produce different-but-still-deterministic hashes -- that
// divergence is expected: PerceptualHash operates on whatever pixels
// image.Decode actually hands it, and compression/quantization changes
// those pixels.
func TestPerceptualHashGoldenVectors(t *testing.T) {
	cases := []struct {
		file string
		want int64
	}{
		{"testdata/gradient.png", -6161209626521968640},
		{"testdata/gradient.jpg", -6164110164506017019},
		{"testdata/gradient.gif", -9190300421549168520},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			data, err := os.ReadFile(c.file)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			img, _, err := image.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("decode fixture: %v", err)
			}
			got, err := PerceptualHash(img)
			if err != nil {
				t.Fatalf("PerceptualHash: %v", err)
			}
			if got != c.want {
				t.Errorf("PerceptualHash(%s) = %d, want %d", c.file, got, c.want)
			}
		})
	}
}

// TestPerceptualHashDeterministic proves hashing the same decoded image
// twice gives the same result -- goimagehash.PerceptionHash has no hidden
// randomness this package's callers could be surprised by.
func TestPerceptualHashDeterministic(t *testing.T) {
	data, err := os.ReadFile("testdata/gradient.png")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	h1, err := PerceptualHash(img)
	if err != nil {
		t.Fatalf("PerceptualHash: %v", err)
	}
	h2, err := PerceptualHash(img)
	if err != nil {
		t.Fatalf("PerceptualHash: %v", err)
	}
	if h1 != h2 {
		t.Errorf("hashing the same image twice gave different results: %d vs %d", h1, h2)
	}
}
