package luminar

import "testing"

func TestSplitDerivative(t *testing.T) {
	suffixes := DefaultDerivativeSuffixes

	cases := []struct {
		name     string
		fileName string
		wantBase string
		wantSuf  string
		wantOK   bool
	}{
		{"upscale, verified case", "IMG_1767_upscale.jpg", "img_1767", "_upscale", true},
		{"panorama, verified case", "DJI_20260824170503_0008_D_PANORAMA.tiff", "dji_20260824170503_0008_d", "_panorama", true},
		{"case-insensitive suffix", "img_1767_UPSCALE.JPG", "img_1767", "_upscale", true},
		{"no suffix", "IMG_1767.jpeg", "", "", false},
		{"suffix mid-name must not match", "photo_upscale_final.jpg", "", "", false},
		{"suffix as whole stem, nothing before it", "_upscale.jpg", "", "_upscale", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base, suf, ok := splitDerivative(tc.fileName, suffixes)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if base != tc.wantBase {
				t.Errorf("base = %q, want %q", base, tc.wantBase)
			}
			if suf != tc.wantSuf {
				t.Errorf("suffix = %q, want %q", suf, tc.wantSuf)
			}
		})
	}
}

func mounted(id, fileName string, camera string, captureTime int64) CatalogImage {
	return CatalogImage{
		ImageID:     id,
		VolumeMount: "/",
		DirPath:     "Pictures",
		FileName:    fileName,
		CameraModel: camera,
		CaptureTime: captureTime,
	}
}

func TestPairDerivativesExactlyOneSourceMatch(t *testing.T) {
	images := []CatalogImage{
		mounted("1", "IMG_1767.jpeg", "iPhone 17 Pro", 100),
		mounted("2", "IMG_1767_upscale.jpg", "iPhone 17 Pro", 100),
	}
	pairs, amb := PairDerivatives(images, DefaultDerivativeSuffixes)
	if len(amb) != 0 {
		t.Fatalf("expected no ambiguities, got %+v", amb)
	}
	if len(pairs) != 1 {
		t.Fatalf("expected exactly 1 pair, got %d: %+v", len(pairs), pairs)
	}
	p := pairs[0]
	if p.SourceRowID != "1" || p.EditRowID != "2" {
		t.Errorf("row ids = (%q, %q), want (1, 2)", p.SourceRowID, p.EditRowID)
	}
	if !p.CameraModelMatch {
		t.Error("CameraModelMatch = false, want true (both Pixel/iPhone models match)")
	}
	if !p.CaptureTimeMatch {
		t.Error("CaptureTimeMatch = false, want true (both capture times match)")
	}
}

// TestPairDerivativesCaptureTimeIsEvidenceNotAGate is the real-catalog case
// (the Panorama Stitching pair): camera model agrees, capture time does not.
// The pair must still be emitted -- gating on capture-time agreement would
// silently drop a true pair.
func TestPairDerivativesCaptureTimeIsEvidenceNotAGate(t *testing.T) {
	images := []CatalogImage{
		mounted("1", "DJI_0008_D.JPG", "FC9470", 200),
		mounted("2", "DJI_0008_D_PANORAMA.tiff", "FC9470", 999),
	}
	pairs, amb := PairDerivatives(images, DefaultDerivativeSuffixes)
	if len(amb) != 0 {
		t.Fatalf("expected no ambiguities, got %+v", amb)
	}
	if len(pairs) != 1 {
		t.Fatalf("expected exactly 1 pair despite the capture-time mismatch, got %d: %+v", len(pairs), pairs)
	}
	p := pairs[0]
	if !p.CameraModelMatch {
		t.Error("CameraModelMatch = false, want true")
	}
	if p.CaptureTimeMatch {
		t.Error("CaptureTimeMatch = true, want false -- this pair's real evidence disagrees on capture time")
	}
}

func TestPairDerivativesAmbiguousStemNotPaired(t *testing.T) {
	images := []CatalogImage{
		mounted("1", "IMG_3000.jpeg", "", 0),
		mounted("2", "IMG_3000.jpeg", "", 0), // second image sharing the exact same stem
		mounted("3", "IMG_3000_upscale.jpg", "", 0),
	}
	pairs, amb := PairDerivatives(images, DefaultDerivativeSuffixes)
	if len(pairs) != 0 {
		t.Fatalf("expected no pairs for an ambiguous stem, got %+v", pairs)
	}
	if len(amb) != 1 {
		t.Fatalf("expected exactly 1 ambiguity, got %d: %+v", len(amb), amb)
	}
	if amb[0].Reason != ReasonAmbiguous || amb[0].Matches != 2 {
		t.Errorf("ambiguity = %+v, want Reason=ReasonAmbiguous Matches=2", amb[0])
	}
}

func TestPairDerivativesNoSourceNotPaired(t *testing.T) {
	images := []CatalogImage{
		mounted("1", "IMG_4000_upscale.jpg", "", 0),
	}
	pairs, amb := PairDerivatives(images, DefaultDerivativeSuffixes)
	if len(pairs) != 0 {
		t.Fatalf("expected no pairs when no source image exists, got %+v", pairs)
	}
	if len(amb) != 1 || amb[0].Reason != ReasonNoSource || amb[0].Matches != 0 {
		t.Fatalf("ambiguity = %+v, want Reason=ReasonNoSource Matches=0", amb)
	}
}

func TestPairDerivativesUnresolvablePathNotPaired(t *testing.T) {
	unmounted := func(id, fileName string) CatalogImage {
		return CatalogImage{ImageID: id, DirPath: "Pictures", FileName: fileName} // VolumeMount left empty
	}
	images := []CatalogImage{
		unmounted("1", "IMG_5000.jpeg"),
		unmounted("2", "IMG_5000_upscale.jpg"),
	}
	pairs, amb := PairDerivatives(images, DefaultDerivativeSuffixes)
	if len(pairs) != 0 {
		t.Fatalf("expected no pairs when neither side has a resolvable path, got %+v", pairs)
	}
	if len(amb) != 1 || amb[0].Reason != ReasonPathUnresolvable {
		t.Fatalf("ambiguity = %+v, want Reason=ReasonPathUnresolvable", amb)
	}
}

func TestPairDerivativesNoFalsePositivesAcrossUnrelatedImages(t *testing.T) {
	// A pile of ordinary, unrelated filenames with no derivative suffix at
	// all must never produce a pair or an ambiguity.
	images := []CatalogImage{
		mounted("1", "IMG_0001.jpeg", "Pixel", 1),
		mounted("2", "IMG_0002.jpeg", "Pixel", 2),
		mounted("3", "DSC_0001.NEF", "Nikon", 3),
		mounted("4", "PXL_20260101_000000.dng", "Pixel", 4),
	}
	pairs, amb := PairDerivatives(images, DefaultDerivativeSuffixes)
	if len(pairs) != 0 || len(amb) != 0 {
		t.Fatalf("expected no pairs or ambiguities among unrelated files, got pairs=%+v amb=%+v", pairs, amb)
	}
}

func TestPairDerivativesCustomSuffix(t *testing.T) {
	images := []CatalogImage{
		mounted("1", "IMG_0001.jpeg", "", 0),
		mounted("2", "IMG_0001_hdr.jpg", "", 0),
	}
	// The default list doesn't include "_hdr" -- an operator-supplied
	// override is what's needed to pair this, exercising the same knob
	// -derivative-suffixes wires up.
	if pairs, _ := PairDerivatives(images, DefaultDerivativeSuffixes); len(pairs) != 0 {
		t.Fatalf("expected no pairs with the default suffix list, got %+v", pairs)
	}
	pairs, amb := PairDerivatives(images, []string{"_hdr"})
	if len(amb) != 0 {
		t.Fatalf("expected no ambiguities, got %+v", amb)
	}
	if len(pairs) != 1 || pairs[0].Suffix != "_hdr" {
		t.Fatalf("expected exactly 1 pair with suffix _hdr, got %+v", pairs)
	}
}
