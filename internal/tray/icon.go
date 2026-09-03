package tray

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// trayIconSize is the tray icon's square dimension in pixels. 32px reads
// cleanly at both standard and HiDPI tray-icon scales on both target
// platforms.
const trayIconSize = 32

// supersample is the per-axis oversampling factor used to antialias the
// monogram below -- its strokes are thin enough at trayIconSize that naive
// point-sampling produced visibly jagged edges.
const supersample = 4

// Geometry mirrors branchDAM's canonical b-node monogram
// (branchDAM's web/src/assets/brand-mark.svg, a 64x64 viewBox), scaled by
// 0.5 to fit trayIconSize: a stem (the immutable master archive), a bowl
// (a graph node) with a short horizontal edge escaping it, and a satellite
// node at the end of that edge (a derivative asset).
//
// The source SVG's stem is `<rect rx="4.5" width="9" .../>` -- since rx is
// exactly half the width, it's a capsule (a rounded rectangle whose ends
// are full semicircles), not a general rounded rect. That means the stem
// and the horizontal edge are the same primitive -- a stroked line segment
// with round caps -- so this file only needs three shape testers, not four.
//
// This geometry is hand-duplicated (not generated from a shared source) in
// two other places, since neither is Go and pulling in a build step just
// for icon assets would fight this file's whole "no external tool" premise:
//   - internal/tray/assets/index.html's inline <svg> (the status page header)
//   - docs/img/logo.svg (the README logo)
//
// TestBuildTrayIconMatchesExpectedGeometry below is a pixel-sample
// regression test for *this* file's rendering; it can't catch the other two
// drifting, so a geometry change here should be mirrored there by hand.
const (
	stemCX, stemY1, stemY2, stemSW = 5.25, 4.75, 27.25, 4.5
	bowlCX, bowlCY, bowlR, bowlSW  = 14.0, 20.0, 7.25, 3.75
	edgeX1, edgeX2, edgeY, edgeSW  = 21.5, 24.0, 20.0, 3.0
	dotCX, dotCY, dotR             = 27.0, 20.0, 3.0

	// Pause badge geometry in the top-right corner (issue #83).
	pauseBadgeCX, pauseBadgeCY, pauseBadgeR = 25.0, 7.0, 5.5
	pauseBar1CX                             = 23.5
	pauseBar2CX                             = 26.5
	pauseBarY1, pauseBarY2, pauseBarSW      = 4.5, 9.5, 1.5
)

// inCircle reports whether (px,py) falls within a filled circle.
func inCircle(px, py, cx, cy, r float64) bool {
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

// onRing reports whether (px,py) falls within a stroked circle (an annulus)
// of the given center, radius, and stroke width.
func onRing(px, py, cx, cy, r, sw float64) bool {
	dx, dy := px-cx, py-cy
	d := math.Sqrt(dx*dx + dy*dy)
	return math.Abs(d-r) <= sw/2
}

// onCapsule reports whether (px,py) falls within sw/2 of the line segment
// (x1,y1)-(x2,y2) -- equivalently, inside a round-capped stroked line, or a
// rounded rectangle whose corner radius equals half its width/height.
func onCapsule(px, py, x1, y1, x2, y2, sw float64) bool {
	vx, vy := x2-x1, y2-y1
	segLenSq := vx*vx + vy*vy
	t := 0.0
	if segLenSq > 0 {
		t = ((px-x1)*vx + (py-y1)*vy) / segLenSq
		t = math.Max(0, math.Min(1, t))
	}
	cx, cy := x1+t*vx, y1+t*vy
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= (sw/2)*(sw/2)
}

// coverage reports the monogram's coverage fraction (0..1) at output pixel
// (x,y), estimated by testing a supersample x supersample sub-pixel grid.
func coverage(x, y int) float64 {
	var covered int
	for sy := 0; sy < supersample; sy++ {
		for sx := 0; sx < supersample; sx++ {
			px := float64(x) + (float64(sx)+0.5)/supersample
			py := float64(y) + (float64(sy)+0.5)/supersample
			if onCapsule(px, py, stemCX, stemY1, stemCX, stemY2, stemSW) ||
				onRing(px, py, bowlCX, bowlCY, bowlR, bowlSW) ||
				onCapsule(px, py, edgeX1, edgeY, edgeX2, edgeY, edgeSW) ||
				inCircle(px, py, dotCX, dotCY, dotR) {
				covered++
			}
		}
	}
	return float64(covered) / float64(supersample*supersample)
}

// pauseCoverage reports the pause badge background and white bars coverage fractions
// (0..1) at output pixel (x,y).
func pauseCoverage(x, y int) (badgeCover, barCover float64) {
	var badgeCount, barCount int
	for sy := 0; sy < supersample; sy++ {
		for sx := 0; sx < supersample; sx++ {
			px := float64(x) + (float64(sx)+0.5)/supersample
			py := float64(y) + (float64(sy)+0.5)/supersample
			if inCircle(px, py, pauseBadgeCX, pauseBadgeCY, pauseBadgeR) {
				if onCapsule(px, py, pauseBar1CX, pauseBarY1, pauseBar1CX, pauseBarY2, pauseBarSW) ||
					onCapsule(px, py, pauseBar2CX, pauseBarY1, pauseBar2CX, pauseBarY2, pauseBarSW) {
					barCount++
				} else {
					badgeCount++
				}
			}
		}
	}
	tot := float64(supersample * supersample)
	return float64(badgeCount) / tot, float64(barCount) / tot
}

func encodeIco(img *image.RGBA) []byte {
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

func buildIcon(paused bool) []byte {
	img := image.NewRGBA(image.Rect(0, 0, trayIconSize, trayIconSize))
	fg := color.RGBA{R: 0x2b, G: 0xa6, B: 0x9a, A: 0xff}      // branchDAM teal
	badgeBg := color.RGBA{R: 0xe6, G: 0x95, B: 0x00, A: 0xff} // amber pause badge
	badgeFg := color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff} // white pause bars

	for y := 0; y < trayIconSize; y++ {
		for x := 0; x < trayIconSize; x++ {
			if paused {
				badgeCover, barCover := pauseCoverage(x, y)
				if badgeCover > 0 || barCover > 0 {
					r := float64(badgeBg.R)*badgeCover + float64(badgeFg.R)*barCover
					g := float64(badgeBg.G)*badgeCover + float64(badgeFg.G)*barCover
					b := float64(badgeBg.B)*badgeCover + float64(badgeFg.B)*barCover
					totalA := badgeCover + barCover
					if totalA > 1.0 {
						totalA = 1.0
					}
					img.Set(x, y, color.RGBA{
						R: uint8(r),
						G: uint8(g),
						B: uint8(b),
						A: uint8(255 * totalA),
					})
					continue
				}
			}
			c := coverage(x, y)
			if c == 0 {
				continue
			}
			// Premultiplied-alpha blend of the opaque teal foreground over
			// a transparent background: at partial coverage c, the
			// resulting pixel is (fg.RGB * c, alpha = 255 * c).
			img.Set(x, y, color.RGBA{
				R: uint8(float64(fg.R) * c),
				G: uint8(float64(fg.G) * c),
				B: uint8(float64(fg.B) * c),
				A: uint8(255 * c),
			})
		}
	}

	return encodeIco(img)
}

// buildTrayIcon renders branchDAM's b-node monogram and wraps it as a
// single-image .ico container (a PNG-in-ICO, supported since Windows
// Vista). Generated in Go rather than committed as a binary asset so the
// tray icon needs no external tool, no binary file in the repository, and
// is trivially regenerable/adjustable. fyne.io/systray's own doc comment
// (systray_windows.go's SetIcon) states iconBytes "should be the content
// of .ico for windows and .ico/.jpg/.png" for darwin -- a single .ico
// buffer works unmodified on both of the platforms this file targets (see
// this file's build tag).
func buildTrayIcon() []byte {
	return buildIcon(false)
}

// buildPausedTrayIcon renders branchDAM's monogram overlaid with an amber
// pause badge (issue #83).
func buildPausedTrayIcon() []byte {
	return buildIcon(true)
}
