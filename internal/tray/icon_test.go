package tray

import "testing"

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
