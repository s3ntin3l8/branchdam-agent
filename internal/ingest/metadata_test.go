package ingest

import (
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCapturedAt(t *testing.T) {
	utc := time.UTC
	cases := []struct {
		name string
		row  map[string]any
		want *time.Time
	}{
		{
			name: "prefers SubSecDateTimeOriginal",
			row: map[string]any{
				"Composite:SubSecDateTimeOriginal": "2024:03:20 12:59:17-04:00",
				"EXIF:DateTimeOriginal":            "2020:01:01 00:00:00",
			},
			want: timePtr(time.Date(2024, 3, 20, 12, 59, 17, 0, time.FixedZone("", -4*3600))),
		},
		{
			name: "falls back to DateTimeOriginal+OffsetTimeOriginal",
			row: map[string]any{
				"EXIF:DateTimeOriginal":   "2024:03:20 12:59:17",
				"EXIF:OffsetTimeOriginal": "-04:00",
			},
			want: timePtr(time.Date(2024, 3, 20, 12, 59, 17, 0, time.FixedZone("", -4*3600))),
		},
		{
			name: "falls back to bare DateTimeOriginal parsed as UTC",
			row: map[string]any{
				"EXIF:DateTimeOriginal": "2024:03:20 12:59:17",
			},
			want: timePtr(time.Date(2024, 3, 20, 12, 59, 17, 0, utc)),
		},
		{
			name: "no time fields at all",
			row:  map[string]any{},
			want: nil,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := capturedAt(c.row)
			if (got == nil) != (c.want == nil) {
				t.Fatalf("capturedAt() = %v, want %v", got, c.want)
			}
			if got != nil && !got.Equal(*c.want) {
				t.Errorf("capturedAt() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestMergeSidecarRowSidecarWinsExceptBookkeeping(t *testing.T) {
	row := map[string]any{
		"EXIF:Model":     "RAW-Camera",
		"File:FileSize":  "1234",
		"ExifTool:Error": "",
	}
	sidecar := map[string]any{
		"SourceFile":     "/path/to/sidecar.xmp",
		"EXIF:Model":     "Sidecar-Wins",
		"XMP:Rating":     float64(5),
		"File:FileSize":  "9999", // bookkeeping -- must NOT overwrite row's
		"ExifTool:Error": "boom", // bookkeeping -- must NOT overwrite row's
	}
	mergeSidecarRow(row, sidecar)

	if row["EXIF:Model"] != "Sidecar-Wins" {
		t.Errorf("EXIF:Model = %v, want sidecar value to win", row["EXIF:Model"])
	}
	if row["XMP:Rating"] != float64(5) {
		t.Errorf("XMP:Rating = %v, want 5 merged in from sidecar", row["XMP:Rating"])
	}
	if row["File:FileSize"] != "1234" {
		t.Errorf("File:FileSize = %v, want RAW's own bookkeeping preserved", row["File:FileSize"])
	}
	if row["ExifTool:Error"] != "" {
		t.Errorf("ExifTool:Error = %v, want RAW's own bookkeeping preserved", row["ExifTool:Error"])
	}
	if _, ok := row["SourceFile"]; ok {
		t.Error("SourceFile should never be merged in")
	}
}

func timePtr(t time.Time) *time.Time { return &t }

func requireExiftool(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not found on PATH -- skipping real-binary metadata test")
	}
	return path
}

// TestExifExtractsPromotedColumnsAndSidecarWins writes real EXIF/XMP tags
// (via exiftool itself, the only practical way to produce a valid JPEG with
// real tags) into a fixture JPEG and a sibling .xmp sidecar, then asserts
// Exiftool.Exif extracts every promoted-column field from the conformance
// contract table and that the sidecar's XMP:OriginalDocumentID overrides
// the base file's -- this is the test the parity harness's own
// fixture-generation depends on being correct.
//
// The sidecar-wins probe deliberately uses OriginalDocumentID, not
// CameraModel: exiftool has no way to write a family-1 "EXIF:" grouped tag
// into a standalone .xmp file at all (there is no EXIF segment in an XMP
// sidecar -- confirmed empirically, "0 image files updated" against
// -EXIF:Model), so mergeSidecarRow's row[k]=v overlay never actually
// collides on EXIF:* keys for a real sidecar in practice, on either side of
// this port. XMP:OriginalDocumentID is a key that genuinely can appear
// identically-grouped on both the embedded-XMP-in-JPEG side and the
// standalone-.xmp side, which is what makes it the right field to prove the
// overlay/precedence logic itself, as opposed to any specific tag name.
func TestExifExtractsPromotedColumnsAndSidecarWins(t *testing.T) {
	exiftoolPath := requireExiftool(t)
	dir := t.TempDir()
	jpegPath := filepath.Join(dir, "IMG_0001.jpg")
	if err := makeMinimalJPEG(jpegPath); err != nil {
		t.Fatalf("create fixture jpeg: %v", err)
	}

	writeTags(t, exiftoolPath, jpegPath, map[string]string{
		"EXIF:Model":                   "ILCE-7RM4",
		"EXIF:SerialNumber":            "1234567",
		"EXIF:LensModel":               "FE 24-70mm F2.8 GM",
		"EXIF:DateTimeOriginal":        "2024:03:20 12:59:17",
		"EXIF:OffsetTimeOriginal":      "-04:00",
		"EXIF:GPSLatitude":             "30.335120",
		"EXIF:GPSLatitudeRef":          "N",
		"EXIF:GPSLongitude":            "81.655480",
		"EXIF:GPSLongitudeRef":         "W",
		"XMP-xmpMM:OriginalDocumentID": "base-doc-id",
		"XMP-xmpMM:DocumentID":         "base-doc-id-2",
	})

	e := &Exiftool{path: exiftoolPath}
	res, err := e.Exif(context.Background(), jpegPath)
	if err != nil {
		t.Fatalf("Exif: %v", err)
	}

	if res.CameraModel != "ILCE-7RM4" {
		t.Errorf("CameraModel = %q, want ILCE-7RM4", res.CameraModel)
	}
	if res.CameraSerial != "1234567" {
		t.Errorf("CameraSerial = %q, want 1234567", res.CameraSerial)
	}
	if res.LensModel != "FE 24-70mm F2.8 GM" {
		t.Errorf("LensModel = %q, want FE 24-70mm F2.8 GM", res.LensModel)
	}
	if res.CapturedAt == nil {
		t.Fatal("CapturedAt is nil, want a parsed time")
	}
	if res.GPSLatitude == nil || *res.GPSLatitude <= 0 {
		t.Errorf("GPSLatitude = %v, want positive (N hemisphere, signed decimal)", res.GPSLatitude)
	}
	if res.GPSLongitude == nil || *res.GPSLongitude >= 0 {
		t.Errorf("GPSLongitude = %v, want negative (W hemisphere, signed decimal)", res.GPSLongitude)
	}
	if res.OriginalDocumentID != "base-doc-id" {
		t.Errorf("OriginalDocumentID = %q, want base-doc-id", res.OriginalDocumentID)
	}

	// Now add a sidecar with a different XMP:OriginalDocumentID and confirm
	// the sidecar's value wins.
	sidecar := sidecarPath(jpegPath)
	if err := os.WriteFile(sidecar, []byte(minimalXMP), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTags(t, exiftoolPath, sidecar, map[string]string{
		"XMP-xmpMM:OriginalDocumentID": "sidecar-doc-id",
	})

	res2, err := e.Exif(context.Background(), jpegPath)
	if err != nil {
		t.Fatalf("Exif with sidecar: %v", err)
	}
	if res2.OriginalDocumentID != "sidecar-doc-id" {
		t.Errorf("OriginalDocumentID with sidecar present = %q, want sidecar-doc-id (sidecar-wins)", res2.OriginalDocumentID)
	}
	// Fields the sidecar never touches must survive untouched from the RAW's
	// own read.
	if res2.CameraModel != "ILCE-7RM4" {
		t.Errorf("CameraModel after sidecar merge = %q, want unchanged ILCE-7RM4", res2.CameraModel)
	}
}

func writeTags(t *testing.T, exiftoolPath, path string, tags map[string]string) {
	t.Helper()
	args := []string{"-overwrite_original"}
	for k, v := range tags {
		args = append(args, "-"+k+"="+v)
	}
	args = append(args, "--", path)
	cmd := exec.Command(exiftoolPath, args...) //nolint:gosec // test helper, fixed exiftoolPath + controlled args
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exiftool write tags on %s: %v\n%s", path, err, out)
	}
}

const minimalXMP = `<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>
<x:xmpmeta xmlns:x="adobe:ns:meta/">
 <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
  <rdf:Description rdf:about=""/>
 </rdf:RDF>
</x:xmpmeta>
<?xpacket end="w"?>
`

// makeMinimalJPEG writes a tiny valid JPEG (a 2x2 solid-color image) so
// exiftool has a real file to tag -- Go's stdlib image/jpeg encoder is used
// rather than a hand-rolled byte literal so the file is guaranteed valid.
func makeMinimalJPEG(path string) error {
	f, err := os.Create(path) //nolint:gosec // test fixture path under t.TempDir()
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return encodeTinyJPEG(f)
}

func encodeTinyJPEG(w io.Writer) error {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 60), G: uint8(y * 60), B: 128, A: 255})
		}
	}
	return jpeg.Encode(w, img, &jpeg.Options{Quality: 90})
}
