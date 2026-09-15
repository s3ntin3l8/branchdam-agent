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

func TestICOStructure(t *testing.T) {
	data, err := ICO()
	if err != nil {
		t.Fatalf("ICO(): %v", err)
	}
	if len(data) < 6 {
		t.Fatalf("ICO() too short: %d bytes", len(data))
	}

	reserved := binary.LittleEndian.Uint16(data[0:2])
	kind := binary.LittleEndian.Uint16(data[2:4])
	count := binary.LittleEndian.Uint16(data[4:6])
	if reserved != 0 {
		t.Errorf("ICONDIR reserved = %d, want 0", reserved)
	}
	if kind != 1 {
		t.Errorf("ICONDIR type = %d, want 1 (icon)", kind)
	}
	if int(count) != len(icoSizes) {
		t.Fatalf("ICONDIR count = %d, want %d", count, len(icoSizes))
	}

	wantSizes := make(map[int]bool, len(icoSizes))
	for _, s := range icoSizes {
		wantSizes[s] = true
	}

	const entryLen = 16
	seenSizes := make(map[int]bool)
	for i := 0; i < int(count); i++ {
		off := 6 + i*entryLen
		if off+entryLen > len(data) {
			t.Fatalf("truncated ICONDIRENTRY at index %d", i)
		}
		entry := data[off : off+entryLen]
		width := int(entry[0])
		if width == 0 {
			width = 256
		}
		height := int(entry[1])
		if height == 0 {
			height = 256
		}
		if width != height {
			t.Errorf("entry %d: width %d != height %d", i, width, height)
		}
		if !wantSizes[width] {
			t.Errorf("entry %d: unexpected size %d", i, width)
		}
		seenSizes[width] = true

		bpp := binary.LittleEndian.Uint16(entry[6:8])
		if bpp != 32 {
			t.Errorf("entry %d: bpp = %d, want 32", i, bpp)
		}
		dataSize := binary.LittleEndian.Uint32(entry[8:12])
		dataOffset := binary.LittleEndian.Uint32(entry[12:16])
		if int(dataOffset)+int(dataSize) > len(data) {
			t.Fatalf("entry %d: data range [%d:%d] exceeds file length %d", i, dataOffset, dataOffset+dataSize, len(data))
		}
		payload := data[dataOffset : dataOffset+dataSize]
		img, err := png.Decode(bytes.NewReader(payload))
		if err != nil {
			t.Fatalf("entry %d payload did not decode as PNG: %v", i, err)
		}
		if b := img.Bounds(); b.Dx() != width || b.Dy() != height {
			t.Errorf("entry %d decoded to %dx%d, want %dx%d", i, b.Dx(), b.Dy(), width, height)
		}
	}
	for size := range wantSizes {
		if !seenSizes[size] {
			t.Errorf("ico output missing expected size %d", size)
		}
	}
}
