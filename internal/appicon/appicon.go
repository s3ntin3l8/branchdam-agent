// Package appicon renders branchDAM's b-node monogram as a macOS .icns
// application icon, entirely in pure Go -- no ImageMagick, no iconutil, no
// binary asset committed to the repo. This mirrors the "no external tool"
// discipline internal/tray/icon.go's own package comment documents; that
// file renders a single 32px tray glyph, this one renders the larger sizes
// a real app icon needs (Finder, Dock, the DMG window,
// Info.plist's CFBundleIconFile) and packages them into the actual .icns
// container format.
//
// The monogram geometry is the same b-node mark as internal/tray/icon.go,
// expressed as fractions of the source SVG's 64x64 viewBox so it scales
// cleanly to any output size. Hand-duplicated rather than shared, for the
// same reason icon.go gives for its own duplication against the status
// page's inline <svg> and docs/img/logo.svg: neither of those call sites is
// Go, and pulling in a build step just for icon assets would fight the
// whole point of rendering icons in Go in the first place. A geometry
// change in any of these files should be mirrored by hand in the others.
package appicon

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Foreground is the same branchDAM teal internal/tray/icon.go uses.
var Foreground = color.RGBA{R: 0x2b, G: 0xa6, B: 0x9a, A: 0xff}

// supersample mirrors internal/tray/icon.go's own antialiasing factor --
// the monogram's strokes are thin enough that naive point-sampling produces
// visibly jagged edges even at the larger sizes this package renders.
const supersample = 4

// Geometry, as fractions of internal/tray/icon.go's 32-unit canvas (itself
// half of the source SVG's 64x64 viewBox -- see that file's own comment).
// Expressing everything as a fraction is what lets Render scale to any
// output size without re-deriving the geometry per size.
const (
	stemCXf, stemY1f, stemY2f, stemSWf = 5.25 / 32.0, 4.75 / 32.0, 27.25 / 32.0, 4.5 / 32.0
	bowlCXf, bowlCYf, bowlRf, bowlSWf  = 14.0 / 32.0, 20.0 / 32.0, 7.25 / 32.0, 3.75 / 32.0
	edgeX1f, edgeX2f, edgeYf, edgeSWf  = 21.5 / 32.0, 24.0 / 32.0, 20.0 / 32.0, 3.0 / 32.0
	dotCXf, dotCYf, dotRf              = 27.0 / 32.0, 20.0 / 32.0, 3.0 / 32.0
)

func inCircle(px, py, cx, cy, r float64) bool {
	dx, dy := px-cx, py-cy
	return dx*dx+dy*dy <= r*r
}

func onRing(px, py, cx, cy, r, sw float64) bool {
	dx, dy := px-cx, py-cy
	d := math.Sqrt(dx*dx + dy*dy)
	return math.Abs(d-r) <= sw/2
}

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
// (x,y) on a size x size canvas, estimated by testing a supersample x
// supersample sub-pixel grid -- the same technique internal/tray/icon.go
// uses, parameterized by size instead of fixed at 32.
func coverage(x, y, size int) float64 {
	s := float64(size)
	stemCX, stemY1, stemY2, stemSW := stemCXf*s, stemY1f*s, stemY2f*s, stemSWf*s
	bowlCX, bowlCY, bowlR, bowlSW := bowlCXf*s, bowlCYf*s, bowlRf*s, bowlSWf*s
	edgeX1, edgeX2, edgeY, edgeSW := edgeX1f*s, edgeX2f*s, edgeYf*s, edgeSWf*s
	dotCX, dotCY, dotR := dotCXf*s, dotCYf*s, dotRf*s

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

// Render draws the monogram in Foreground on a transparent size x size
// canvas.
func Render(size int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			c := coverage(x, y, size)
			if c == 0 {
				continue
			}
			// Premultiplied-alpha blend of the opaque foreground over a
			// transparent background, matching internal/tray/icon.go's own
			// blend (image.RGBA.Set expects premultiplied values).
			img.Set(x, y, color.RGBA{
				R: uint8(float64(Foreground.R) * c),
				G: uint8(float64(Foreground.G) * c),
				B: uint8(float64(Foreground.B) * c),
				A: uint8(255 * c),
			})
		}
	}
	return img
}

// PNG renders the monogram at size and encodes it as PNG bytes.
func PNG(size int) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, Render(size)); err != nil {
		return nil, fmt.Errorf("appicon: encode %dx%d png: %w", size, size, err)
	}
	return buf.Bytes(), nil
}

// icnsSlot is one entry in the .icns container: an OSType tag paired with
// the pixel size to render at. This is the same {16,32,64,128,256,512,1024}
// PNG-payload slot set iconutil itself produces from a standard .iconset
// directory (Apple Icon Image format, PNG-payload OSTypes introduced in Mac
// OS X 10.7) -- some pixel sizes are deliberately rendered twice under
// different tags (e.g. 32x32 is both the "32pt@1x" icp5 slot and the
// "16pt@2x" ic11 slot), matching what a real .iconset with only
// non-"@2x"-suffixed files at each pt size, plus their @2x counterparts,
// would contain.
type icnsSlot struct {
	tag  string
	size int
}

var icnsSlots = []icnsSlot{
	{"icp4", 16},   // 16x16
	{"icp5", 32},   // 32x32
	{"ic11", 32},   // 16x16@2x
	{"icp6", 64},   // 64x64
	{"ic12", 64},   // 32x32@2x
	{"ic07", 128},  // 128x128
	{"ic08", 256},  // 256x256
	{"ic13", 256},  // 128x128@2x
	{"ic09", 512},  // 512x512
	{"ic14", 512},  // 256x256@2x
	{"ic10", 1024}, // 512x512@2x
}

// ICNS renders the monogram at every size icnsSlots needs and packages the
// result as a complete .icns file: a 4-byte "icns" magic, a 4-byte
// big-endian total length, then one TLV block per slot (4-byte OSType tag +
// 4-byte big-endian block length including its own 8-byte header + PNG
// bytes). Each distinct pixel size is rendered once and reused across every
// slot that needs it, rather than re-rendered per slot.
func ICNS() ([]byte, error) {
	rendered := make(map[int][]byte)
	sizes := make(map[int]struct{})
	for _, slot := range icnsSlots {
		sizes[slot.size] = struct{}{}
	}
	for size := range sizes {
		png, err := PNG(size)
		if err != nil {
			return nil, err
		}
		rendered[size] = png
	}

	var body bytes.Buffer
	for _, slot := range icnsSlots {
		data := rendered[slot.size]
		if len(slot.tag) != 4 {
			return nil, fmt.Errorf("appicon: invalid icns tag %q", slot.tag)
		}
		body.WriteString(slot.tag)
		if err := binary.Write(&body, binary.BigEndian, uint32(len(data)+8)); err != nil {
			return nil, fmt.Errorf("appicon: write icns block length: %w", err)
		}
		body.Write(data)
	}

	var out bytes.Buffer
	out.WriteString("icns")
	if err := binary.Write(&out, binary.BigEndian, uint32(body.Len()+8)); err != nil {
		return nil, fmt.Errorf("appicon: write icns total length: %w", err)
	}
	out.Write(body.Bytes())
	return out.Bytes(), nil
}
