package main

import (
	"context"
	"testing"
)

func TestFormatByteSize(t *testing.T) {
	for _, tc := range []struct {
		bytes uint64
		want  string
	}{
		{500, "500 B"},
		{1024, "1 KB"},
		{1536, "1.5 KB"},
		{1048576, "1 MB"},
		{1073741824, "1 GB"},
		{34359738368, "32 GB"},
		{1099511627776, "1 TB"},
	} {
		if got := formatByteSize(tc.bytes); got != tc.want {
			t.Errorf("formatByteSize(%d) = %q, want %q", tc.bytes, got, tc.want)
		}
	}
}

func TestLookupVolumeLabelAndSizeFallback(t *testing.T) {
	// For a path that doesn't exist on host, label should fall back to filepath.Base
	label, _ := lookupVolumeLabelAndSize(context.Background(), "/Volumes/CANON_R5")
	if label != "CANON_R5" {
		t.Errorf("lookupVolumeLabelAndSize label = %q, want %q", label, "CANON_R5")
	}
}
