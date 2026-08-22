package ingest

import (
	"os"
	"path/filepath"
	"testing"
)

const sampleSRTWithFix = `1
00:00:00,000 --> 00:00:00,033
<font size="28">FrameCnt: 1, DiffTime: 33ms
2024-03-20 12:59:17,819
[iso: 400] [shutter: 1/320.0] [fnum: 1.7] [ev: 2.0] [color_md: default] [focal_len: 36.30] [latitude: 30.335120] [longitude: -81.655480] [rel_alt: 6.500 abs_alt: -32.309] [ct: 5695]</font>
`

const sampleSRTPreLockZero = `1
00:00:00,000 --> 00:00:00,033
<font size="28">FrameCnt: 1, DiffTime: 33ms
2024-03-20 12:59:16,000
[iso: 100] [shutter: 1/500.0] [fnum: 2.8] [ev: 0.0] [latitude: 0.000000] [longitude: 0.000000] [rel_alt: 0.000 abs_alt: 0.000]</font>
`

func TestFindSRTSidecarLowercaseExt(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "DJI_0001.MP4")
	srt := filepath.Join(dir, "DJI_0001.srt")
	if err := os.WriteFile(srt, []byte(sampleSRTWithFix), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := findSRTSidecar(video)
	if !ok {
		t.Fatal("expected sidecar to be found")
	}
	if got != srt {
		t.Errorf("got %q, want %q", got, srt)
	}
}

func TestFindSRTSidecarUppercaseExt(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "DJI_0002.MP4")
	srt := filepath.Join(dir, "DJI_0002.SRT")
	if err := os.WriteFile(srt, []byte(sampleSRTWithFix), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := findSRTSidecar(video)
	if !ok {
		t.Fatal("expected sidecar to be found")
	}
	if got != srt {
		t.Errorf("got %q, want %q", got, srt)
	}
}

func TestFindSRTSidecarMissing(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "DJI_0003.MP4")
	if _, ok := findSRTSidecar(video); ok {
		t.Error("expected no sidecar to be found")
	}
}

func TestFindSRTSidecarDoesNotMatchOtherStem(t *testing.T) {
	dir := t.TempDir()
	video := filepath.Join(dir, "DJI_0004.MP4")
	// A same-directory .srt for a DIFFERENT stem must not be picked up.
	if err := os.WriteFile(filepath.Join(dir, "DJI_0005.srt"), []byte(sampleSRTWithFix), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := findSRTSidecar(video); ok {
		t.Error("expected no match for a differently-stemmed .srt")
	}
}

func TestSRTGPSValidFix(t *testing.T) {
	dir := t.TempDir()
	srtPath := filepath.Join(dir, "DJI_0001.srt")
	if err := os.WriteFile(srtPath, []byte(sampleSRTWithFix), 0o644); err != nil {
		t.Fatal(err)
	}
	lat, lon, ok, err := srtGPS(srtPath)
	if err != nil {
		t.Fatalf("srtGPS: %v", err)
	}
	if !ok {
		t.Fatal("expected ok=true for a valid fix")
	}
	if lat != 30.335120 || lon != -81.655480 {
		t.Errorf("lat/lon = %v/%v, want 30.335120/-81.655480", lat, lon)
	}
}

func TestSRTGPSPreLockZeroRejected(t *testing.T) {
	// The (0,0) placeholder fix before GPS lock must not be returned as a
	// real GPS point -- djisrt.ParseFirstPoint's own isValidFix rejects it,
	// and srtGPS surfaces that as ok=false, not an error.
	dir := t.TempDir()
	srtPath := filepath.Join(dir, "prelock.srt")
	if err := os.WriteFile(srtPath, []byte(sampleSRTPreLockZero), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := srtGPS(srtPath)
	if err != nil {
		t.Fatalf("srtGPS: %v", err)
	}
	if ok {
		t.Error("expected ok=false for a (0,0) pre-lock placeholder fix")
	}
}

func TestSRTGPSMissingFile(t *testing.T) {
	_, _, ok, err := srtGPS(filepath.Join(t.TempDir(), "does-not-exist.srt"))
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if ok {
		t.Error("ok must be false on error")
	}
}

func TestSRTGPSEmptyFileNoGPSData(t *testing.T) {
	dir := t.TempDir()
	srtPath := filepath.Join(dir, "empty.srt")
	if err := os.WriteFile(srtPath, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, ok, err := srtGPS(srtPath)
	if err != nil {
		t.Fatalf("srtGPS on an empty file should be a clean ok=false, not an error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for an empty file with no GPS data")
	}
}
