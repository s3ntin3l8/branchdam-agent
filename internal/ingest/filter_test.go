package ingest

import (
	"testing"
)

// TestShouldSkipByName pins the OS-metadata skip list. The walk applies
// this BEFORE the AllowedExtensions check, so every name match
// unconditionally short-circuits the per-file pipeline (no exiftool fork,
// no DualWrite, no EVENT_NODE_CREATED).
func TestShouldSkipByName(t *testing.T) {
	cases := []struct {
		label  string
		path   string
		skip   bool
		reason string
	}{
		{"Thumbs.db", "Thumbs.db", true, "Windows thumbnail cache"},
		{"thumbs.db lowercase", "thumbs.db", true, "case-insensitive Windows thumbnail cache"},
		{"THUMBS.DB uppercase", "THUMBS.DB", true, "case-insensitive Windows thumbnail cache"},
		{"System Volume Information", "System Volume Information", true, "Windows NTFS metadata folder"},
		{"system volume information lowercase", "system volume information", true, "case-insensitive Windows NTFS metadata folder"},
		{".DS_Store", ".DS_Store", true, "macOS Finder metadata"},
		{"AppleDouble", "._IMG_0001.ARW", true, "macOS AppleDouble sidecar (leading dot)"},
		{"dotfile", ".hidden", true, "any dotfile"},
		{"dotfile in subdir", ".dotdir/.DS_Store", true, "dotfile basename in a subdir"},
		{"normal photo", "IMG_0001.jpg", false, "normal photo"},
		{"normal sidecar", "sidecar.xmp", false, "normal sidecar (subject to extension filter below)"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			gotSkip, gotReason := shouldSkipByName(c.path)
			if gotSkip != c.skip {
				t.Errorf("path %q: got skip=%v, want %v (%s)", c.path, gotSkip, c.skip, c.reason)
			}
			if c.skip && gotReason == "" {
				t.Errorf("path %q: expected a non-empty reason when skipping", c.path)
			}
		})
	}
}

// TestShouldSkipByExtension covers the case-insensitive
// AllowedExtensions comparison. An empty/nil list means "accept all"
// (caller short-circuits before this is invoked, but the function must
// also behave correctly if called directly).
func TestShouldSkipByExtension(t *testing.T) {
	cases := []struct {
		label    string
		allowed  []string
		ext      string
		wantSkip bool
	}{
		{"nil list, jpg", nil, "jpg", false},
		{"empty list, jpg", []string{}, "jpg", false},
		{"single jpg match", []string{"jpg"}, "jpg", false},
		{"single JPG match, file jpg", []string{"JPG"}, "jpg", false},
		{"single jpg match, file JPG", []string{"jpg"}, "JPG", false},
		{"multi list match", []string{"jpg", "mp4"}, "mp4", false},
		{"single jpg, file png", []string{"jpg"}, "png", true},
		{"single jpg, file txt", []string{"jpg"}, "txt", true},
		{"single jpg, file no ext", []string{"jpg"}, "", true},
		{"leading dot in list, file jpg", []string{".jpg"}, "jpg", false},
		{"leading dot JPG in list, file jpg", []string{".JPG"}, "jpg", false},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			got := shouldSkipByExtension(c.allowed, c.ext)
			if got != c.wantSkip {
				t.Errorf("allowed=%v ext=%q: got skip=%v, want %v", c.allowed, c.ext, got, c.wantSkip)
			}
		})
	}
}
