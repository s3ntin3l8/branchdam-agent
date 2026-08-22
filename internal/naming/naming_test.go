package naming

import "testing"

// TestStem's cases are transcribed verbatim from branchDAM's own committed
// internal/naming/naming_test.go (TestStem, commit c570690) -- that table
// was produced by running branchDAM's actual Stem implementation, so this
// is a copy of an already-generated golden table, not a hand-computed one.
// Reconfirmed by running `go test ./internal/naming/...` in a checkout of
// the branchdam repo at that commit before transcribing.
func TestStem(t *testing.T) {
	cases := map[string]string{
		"DSC01234.ARW":        "dsc01234",
		"DSC01234_edited.jpg": "dsc01234_edited", // not a recognized suffix -- only the exact patterns below are stripped
		"render_v1_proxy.jpg": "render",
		"IMG_0001-2.jpg":      "img_0001",
		"IMG_0001 copy.jpg":   "img_0001",
		"IMG_0001(1).jpg":     "img_0001",
		"plain.jpg":           "plain",
		"no_extension_at_all": "no_extension_at_all",

		// H3 regression: a camera's own hyphen-numbered default naming
		// (Sony DSC-NNNN, some IMG-NNNN variants) must NOT collapse to a
		// bare "dsc"/"img" shared by every file in the shoot -- the -\d+
		// suffix branch is bounded to 1-2 digits precisely so these stay
		// distinct.
		"DSC-0001.JPG": "dsc-0001",
		"IMG-1234.jpg": "img-1234",
		// The genuine "-N" duplicate-index case (OS auto-renaming, always
		// small) is still stripped.
		"photo-2.jpg":  "photo",
		"photo-12.jpg": "photo",
		// 3+ digits after a hyphen reads as a camera serial/frame number,
		// not a duplicate index, and is deliberately left alone.
		"photo-123.jpg": "photo-123",

		// An unpadded 1-2 digit hyphen-numbering scheme still reads as the
		// "-N duplicate index" pattern and collapses to a shared stem.
		"DSC-01.JPG": "dsc",
		"DSC-99.JPG": "dsc",
	}
	for in, want := range cases {
		if got := Stem(in); got != want {
			t.Errorf("Stem(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSuffixKind's cases are transcribed verbatim from branchDAM's own
// committed internal/naming/naming_test.go (TestSuffixKind, commit c570690),
// same provenance as TestStem above.
func TestSuffixKind(t *testing.T) {
	cases := map[string]SuffixKind{
		// index markers -- duplicate-index suffixes
		"DSC-01.JPG":      SuffixIndex,
		"photo-2.jpg":     SuffixIndex,
		"photo-12.jpg":    SuffixIndex,
		"IMG_0001-2.jpg":  SuffixIndex,
		"IMG_0001(1).jpg": SuffixIndex,

		// role markers -- derivation-role suffixes
		"render_v1_proxy.jpg": SuffixRole,
		"IMG_0001 copy.jpg":   SuffixRole,

		// nothing stripped
		"DSC-0001.JPG":  SuffixNone,
		"plain.jpg":     SuffixNone,
		"photo-123.jpg": SuffixNone,

		// mixed: both an index and a role marker stripped -- index
		// ambiguity dominates regardless of which marker was adjacent to
		// the stem in the original filename.
		"photo-2_edit.jpg": SuffixIndex,
		"photo_edit-2.jpg": SuffixIndex,
	}
	for in, want := range cases {
		if got := Kind(in); got != want {
			t.Errorf("Kind(%q) = %v, want %v", in, got, want)
		}
	}
}
