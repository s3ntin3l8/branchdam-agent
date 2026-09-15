package appicon

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"testing"
)

func TestPNGDecodesAtEachSize(t *testing.T) {
	for _, size := range []int{16, 32, 64, 128, 256, 512, 1024} {
		data, err := PNG(size)
		if err != nil {
			t.Fatalf("PNG(%d): %v", size, err)
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("PNG(%d) did not decode: %v", size, err)
		}
		b := img.Bounds()
		if b.Dx() != size || b.Dy() != size {
			t.Errorf("PNG(%d) decoded to %dx%d, want %dx%d", size, b.Dx(), b.Dy(), size, size)
		}
	}
}

func TestPNGHasCoverage(t *testing.T) {
	// A blank (all-transparent) render would mean the geometry fractions
	// resolved to nothing on-canvas -- catch that rather than silently
	// shipping an empty icon.
	img := Render(256)
	var opaque int
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a > 0 {
				opaque++
			}
		}
	}
	if opaque == 0 {
		t.Fatal("Render(256) produced no covered pixels")
	}
}

func TestICNSStructure(t *testing.T) {
	data, err := ICNS()
	if err != nil {
		t.Fatalf("ICNS(): %v", err)
	}
	if len(data) < 8 || string(data[:4]) != "icns" {
		t.Fatalf("ICNS() missing magic header, got %q", data[:min(8, len(data))])
	}
	totalLen := binary.BigEndian.Uint32(data[4:8])
	if int(totalLen) != len(data) {
		t.Errorf("ICNS() total length field = %d, want %d (actual byte count)", totalLen, len(data))
	}

	// Walk every TLV block and confirm each one's declared length is
	// consistent and its payload decodes as PNG at the size its tag
	// implies.
	wantSize := make(map[string]int, len(icnsSlots))
	for _, slot := range icnsSlots {
		wantSize[slot.tag] = slot.size
	}

	off := 8
	seen := map[string]bool{}
	for off < len(data) {
		if off+8 > len(data) {
			t.Fatalf("truncated icns block header at offset %d", off)
		}
		tag := string(data[off : off+4])
		blockLen := binary.BigEndian.Uint32(data[off+4 : off+8])
		if int(blockLen) < 8 || off+int(blockLen) > len(data) {
			t.Fatalf("block %q at offset %d has invalid length %d", tag, off, blockLen)
		}
		payload := data[off+8 : off+int(blockLen)]
		size, known := wantSize[tag]
		if !known {
			t.Fatalf("unexpected icns tag %q", tag)
		}
		img, err := png.Decode(bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("block %q payload did not decode as PNG: %v", tag, err)
		}
		if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
			t.Errorf("block %q decoded to %dx%d, want %dx%d", tag, b.Dx(), b.Dy(), size, size)
		}
		seen[tag] = true
		off += int(blockLen)
	}
	for tag := range wantSize {
		if !seen[tag] {
			t.Errorf("icns output missing expected block %q", tag)
		}
	}
}
