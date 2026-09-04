package tray

import (
	"bytes"
	"image"
	"image/png"
	"testing"
)

func TestBuildTrayIconIsAValidICOContainer(t *testing.T) {
	b := buildTrayIcon()
	if len(b) < 6+16 {
		t.Fatalf("icon too short: %d bytes", len(b))
	}
	// ICONDIR: reserved=0, type=1 (icon), count=1 (little-endian uint16 each).
	if b[0] != 0 || b[1] != 0 {
		t.Errorf("reserved field = %v, want 0,0", b[0:2])
	}
	if b[2] != 1 || b[3] != 0 {
		t.Errorf("type field = %v, want 1,0 (icon)", b[2:4])
	}
	if b[4] != 1 || b[5] != 0 {
		t.Errorf("count field = %v, want 1,0", b[4:6])
	}
	// ICONDIRENTRY width/height should match trayIconSize.
	if b[6] != byte(trayIconSize) || b[7] != byte(trayIconSize) {
		t.Errorf("width/height = %d,%d, want %d,%d", b[6], b[7], trayIconSize, trayIconSize)
	}
	// PNG magic bytes should start right after the 22-byte header.
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	got := b[22 : 22+len(pngMagic)]
	for i, want := range pngMagic {
		if got[i] != want {
			t.Fatalf("PNG magic mismatch at byte %d: got %x, want %x", i, got, pngMagic)
		}
	}
}

// TestBuildTrayIconMatchesExpectedGeometry decodes the actual rendered PNG
// and samples pixels at points chosen to be well inside or outside each
// shape (not near an edge, so antialiasing can't make the assertion flaky).
// TestBuildTrayIconIsAValidICOContainer only validates the .ico wrapper
// bytes -- this is the test that would catch a typo'd geometry constant
// silently blanking or misplacing part of the monogram.
func TestBuildTrayIconMatchesExpectedGeometry(t *testing.T) {
	b := buildTrayIcon()
	img, err := png.Decode(bytes.NewReader(b[22:]))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}

	opaque := func(t *testing.T, name string, x, y int) {
		t.Helper()
		_, _, _, a := img.At(x, y).RGBA()
		if a == 0 {
			t.Errorf("%s at (%d,%d): alpha = 0, want opaque", name, x, y)
		}
	}
	transparent := func(t *testing.T, name string, x, y int) {
		t.Helper()
		_, _, _, a := img.At(x, y).RGBA()
		if a != 0 {
			t.Errorf("%s at (%d,%d): alpha = %d, want fully transparent", name, x, y, a)
		}
	}

	// Stem: a vertical capsule centered at x=5.25, spanning y in
	// [4.75,27.25] -- (5,16) is deep inside both bounds.
	opaque(t, "stem", 5, 16)
	// Bowl: a *stroked* ring (not filled) centered at (14,20) with inner
	// radius 5.375 -- the center itself must be hollow.
	transparent(t, "bowl center (hollow ring)", 14, 20)
	// Bowl ring itself: radius 7.25 from center, e.g. straight up from
	// center at (14,13) is on the stroke.
	opaque(t, "bowl ring", 14, 13)
	// Satellite: a filled circle centered at (27,20) -- the center is
	// solid, unlike the bowl.
	opaque(t, "satellite dot", 27, 20)
	// Background corner: far from every shape.
	transparent(t, "background corner", 0, 0)
	if b := img.Bounds(); b != image.Rect(0, 0, trayIconSize, trayIconSize) {
		t.Errorf("decoded image bounds = %v, want %dx%d", b, trayIconSize, trayIconSize)
	}
}

func TestBuildPausedTrayIconIsAValidICOContainer(t *testing.T) {
	b := buildPausedTrayIcon()
	if len(b) < 6+16 {
		t.Fatalf("icon too short: %d bytes", len(b))
	}
	if b[0] != 0 || b[1] != 0 || b[2] != 1 || b[3] != 0 || b[4] != 1 || b[5] != 0 {
		t.Errorf("invalid ICONDIR header: %v", b[0:6])
	}
	if b[6] != byte(trayIconSize) || b[7] != byte(trayIconSize) {
		t.Errorf("width/height = %d,%d, want %d,%d", b[6], b[7], trayIconSize, trayIconSize)
	}
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	got := b[22 : 22+len(pngMagic)]
	for i, want := range pngMagic {
		if got[i] != want {
			t.Fatalf("PNG magic mismatch at byte %d: got %x, want %x", i, got, pngMagic)
		}
	}
}

func TestBuildPausedTrayIconMatchesExpectedGeometry(t *testing.T) {
	b := buildPausedTrayIcon()
	img, err := png.Decode(bytes.NewReader(b[22:]))
	if err != nil {
		t.Fatalf("png.Decode: %v", err)
	}

	opaque := func(t *testing.T, name string, x, y int) {
		t.Helper()
		_, _, _, a := img.At(x, y).RGBA()
		if a == 0 {
			t.Errorf("%s at (%d,%d): alpha = 0, want opaque", name, x, y)
		}
	}

	// Stem: should still be present in paused icon
	opaque(t, "stem", 5, 16)
	// Bowl ring
	opaque(t, "bowl ring", 14, 13)
	// Pause badge area: centered at (25, 7)
	opaque(t, "pause badge", 25, 7)
	// Pause bar
	opaque(t, "pause bar 1", 23, 7)
	opaque(t, "pause bar 2", 26, 7)
}
