package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
)

// trayIconSize is the tray icon's square dimension in pixels. 32px reads
// cleanly at both standard and HiDPI tray-icon scales on both target
// platforms.
const trayIconSize = 32

// buildTrayIcon renders a small solid-color circle and wraps it as a
// single-image .ico container (a PNG-in-ICO, supported since Windows
// Vista). Generated in Go rather than committed as a binary asset so the
// tray icon needs no external tool, no binary file in the repository, and
// is trivially regenerable/adjustable. fyne.io/systray's own doc comment
// (systray_windows.go's SetIcon) states iconBytes "should be the content
// of .ico for windows and .ico/.jpg/.png" for darwin -- a single .ico
// buffer works unmodified on both of the platforms this file targets (see
// this file's build tag).
func buildTrayIcon() []byte {
	img := image.NewRGBA(image.Rect(0, 0, trayIconSize, trayIconSize))
	cx, cy := float64(trayIconSize)/2, float64(trayIconSize)/2
	r := float64(trayIconSize)/2 - 2
	fg := color.RGBA{R: 0x2b, G: 0xa6, B: 0x9a, A: 0xff} // branchDAM teal

	for y := 0; y < trayIconSize; y++ {
		for x := 0; x < trayIconSize; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, fg)
			}
		}
	}

	var pngBuf bytes.Buffer
	// png.Encode only fails on a write error, never on this in-memory
	// buffer -- the error is deliberately not surfaced further so a
	// startup-time icon-render problem can't fail the whole tray (the
	// icon-less fallback is systray's own, not something this package
	// needs to special-case).
	_ = png.Encode(&pngBuf, img)
	pngBytes := pngBuf.Bytes()

	var ico bytes.Buffer
	// ICONDIR (6 bytes): reserved, type=1 (icon), count=1.
	_ = binary.Write(&ico, binary.LittleEndian, uint16(0))
	_ = binary.Write(&ico, binary.LittleEndian, uint16(1))
	_ = binary.Write(&ico, binary.LittleEndian, uint16(1))
	// ICONDIRENTRY (16 bytes).
	ico.WriteByte(byte(trayIconSize))                       // width
	ico.WriteByte(byte(trayIconSize))                       // height
	ico.WriteByte(0)                                        // color count (0 = no palette, >=8bpp)
	ico.WriteByte(0)                                        // reserved
	_ = binary.Write(&ico, binary.LittleEndian, uint16(1))  // color planes
	_ = binary.Write(&ico, binary.LittleEndian, uint16(32)) // bits per pixel
	_ = binary.Write(&ico, binary.LittleEndian, uint32(len(pngBytes)))
	_ = binary.Write(&ico, binary.LittleEndian, uint32(6+16)) // offset to image data
	ico.Write(pngBytes)

	return ico.Bytes()
}
