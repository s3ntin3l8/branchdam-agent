package djisrt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFirstPoint_BracketFormat(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sample.srt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	p, err := ParseFirstPoint(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	// The fixture's first cue (FrameCnt: 1) carries
	// [latitude: 30.335120] [longitude: -81.655480] -- the second and third
	// cues have slightly different values, so returning those instead would
	// mean this test caught ParseFirstPoint reading past the first match.
	if p.Latitude != 30.335120 {
		t.Errorf("Latitude = %v, want 30.335120 (first cue, not a later one)", p.Latitude)
	}
	if p.Longitude != -81.655480 {
		t.Errorf("Longitude = %v, want -81.655480 (first cue, not a later one)", p.Longitude)
	}
}

func TestParseFirstPoint_LegacyGPSParenFormat(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sample_legacy_gps_paren.srt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	p, err := ParseFirstPoint(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	// GPS(longitude,latitude,altitude) -- longitude first, see package doc.
	if p.Latitude != 30.335120 {
		t.Errorf("Latitude = %v, want 30.335120", p.Latitude)
	}
	if p.Longitude != -81.655480 {
		t.Errorf("Longitude = %v, want -81.655480", p.Longitude)
	}
}

func TestParseFirstPoint_NoGPSData(t *testing.T) {
	text := `1
00:00:00,000 --> 00:00:00,033
<font size="28">FrameCnt: 1, DiffTime: 33ms
2024-03-20 12:59:17,819
[iso: 400] [shutter: 1/320.0] [fnum: 1.7]</font>
`
	_, err := ParseFirstPoint(strings.NewReader(text))
	if !errors.Is(err, ErrNoGPSData) {
		t.Fatalf("err = %v, want ErrNoGPSData", err)
	}
}

func TestParseFirstPoint_EmptyInput(t *testing.T) {
	_, err := ParseFirstPoint(strings.NewReader(""))
	if !errors.Is(err, ErrNoGPSData) {
		t.Fatalf("err = %v, want ErrNoGPSData", err)
	}
}

func TestParseFirstPoint_CaseAndSpacingTolerance(t *testing.T) {
	// Observed firmware variance: uppercase keys, a space before the colon.
	text := `[LATITUDE : 12.5] [LONGITUDE : -45.25]`
	p, err := ParseFirstPoint(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	if p.Latitude != 12.5 || p.Longitude != -45.25 {
		t.Errorf("got (%v, %v), want (12.5, -45.25)", p.Latitude, p.Longitude)
	}
}

func TestParseFirstPoint_CRLF(t *testing.T) {
	text := "1\r\n00:00:00,000 --> 00:00:00,033\r\n[latitude: 1.0] [longitude: 2.0]\r\n"
	p, err := ParseFirstPoint(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	if p.Latitude != 1.0 || p.Longitude != 2.0 {
		t.Errorf("got (%v, %v), want (1.0, 2.0)", p.Latitude, p.Longitude)
	}
}

// TestParseFirstPoint_SkipsPreLockZeroZero proves ParseFirstPoint skips
// past DJI's common pre-GPS-lock leading (0,0) placeholder cues and returns
// the aircraft's actual first fix later in the file, rather than returning
// the bogus (0,0) point as if it were real (see isValidFix's doc comment).
func TestParseFirstPoint_SkipsPreLockZeroZero(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sample_prelock_zero.srt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	p, err := ParseFirstPoint(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	if p.Latitude != 30.335120 {
		t.Errorf("Latitude = %v, want 30.335120 (the first post-lock fix, not the (0,0) placeholder)", p.Latitude)
	}
	if p.Longitude != -81.655480 {
		t.Errorf("Longitude = %v, want -81.655480 (the first post-lock fix, not the (0,0) placeholder)", p.Longitude)
	}
}

// TestParseFirstPoint_ZeroZeroOnlyIsNoGPSData proves a file whose ONLY
// coordinates are the pre-lock (0,0) placeholder (GPS lock never acquired
// before the sidecar ends) correctly reports ErrNoGPSData rather than
// returning (0,0) as if it were a real fix.
func TestParseFirstPoint_ZeroZeroOnlyIsNoGPSData(t *testing.T) {
	text := `[latitude: 0.000000] [longitude: 0.000000]`
	_, err := ParseFirstPoint(strings.NewReader(text))
	if !errors.Is(err, ErrNoGPSData) {
		t.Fatalf("err = %v, want ErrNoGPSData", err)
	}
}

// TestParseFirstPoint_OutOfRangeCoordinatesRejected proves a
// syntactically-matched but out-of-range value (not a real decimal-degree
// coordinate) is rejected rather than returned, and parsing continues to
// look for a valid fix.
func TestParseFirstPoint_OutOfRangeCoordinatesRejected(t *testing.T) {
	t.Run("out of range then valid", func(t *testing.T) {
		text := "[latitude: 3012.5] [longitude: 200.0]\n[latitude: 30.335120] [longitude: -81.655480]"
		p, err := ParseFirstPoint(strings.NewReader(text))
		if err != nil {
			t.Fatalf("ParseFirstPoint: %v", err)
		}
		if p.Latitude != 30.335120 || p.Longitude != -81.655480 {
			t.Errorf("got (%v, %v), want the valid second fix (30.335120, -81.655480)", p.Latitude, p.Longitude)
		}
	})

	t.Run("out of range only is ErrNoGPSData", func(t *testing.T) {
		text := "[latitude: 3012.5] [longitude: 200.0]"
		_, err := ParseFirstPoint(strings.NewReader(text))
		if !errors.Is(err, ErrNoGPSData) {
			t.Fatalf("err = %v, want ErrNoGPSData", err)
		}
	})
}

func TestIsValidFix(t *testing.T) {
	cases := []struct {
		name     string
		lat, lon float64
		want     bool
	}{
		{"valid", 30.335120, -81.655480, true},
		{"zero,zero rejected (pre-lock placeholder)", 0, 0, false},
		{"lat too high", 90.1, 0, false},
		{"lat too low", -90.1, 0, false},
		{"lon too high", 0, 180.1, false},
		{"lon too low", 0, -180.1, false},
		{"boundary lat/lon accepted", 90, 180, true},
		{"boundary negative lat/lon accepted", -90, -180, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidFix(tc.lat, tc.lon); got != tc.want {
				t.Errorf("isValidFix(%v, %v) = %v, want %v", tc.lat, tc.lon, got, tc.want)
			}
		})
	}
}
