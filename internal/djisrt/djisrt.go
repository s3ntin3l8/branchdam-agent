// Package djisrt is a byte-for-byte port of branchDAM's own
// internal/djisrt/djisrt.go (commit c570690) -- nothing under branchDAM's
// internal/ is importable cross-module, so this is a copy of the algorithm,
// not the file verbatim (this doc comment is adapted for this repo; the
// regexes, isValidFix thresholds, and ParseFirstPoint logic are unchanged).
// It parses just enough of a DJI drone's ".srt" flight-telemetry sidecar to
// pull out ONE representative GPS point (the first frame's fix) -- see M1's
// issue: the agent puts this on the video's own EVENT_NODE_CREATED, never a
// separate node/edge for the .srt file itself.
//
// DJI (and several similar consumer drones) write a subtitle-formatted
// sidecar alongside the flight video -- one .srt "cue" roughly per second,
// each cue's text line embedding flight telemetry (GPS, altitude, gimbal,
// camera settings) as plain text rather than as real SRT subtitle content.
//
// Format assumption (researched, not guessed): current-generation DJI
// firmware (Mavic/Air/Mini class, roughly 2020+) embeds coordinates as
// bracketed key-value pairs within each cue's text, e.g.
//
//	[iso: 400] [shutter: 1/320.0] [fnum: 1.7] [ev: 2.0] [ct: 5695]
//	[latitude: 30.123456] [longitude: -81.123456] [rel_alt: 6.500 abs_alt: -32.309]
//
// (field set/order varies by model and firmware -- e.g. some place
// latitude/longitude before the altitude fields, some include a
// [focal_len: ...] entry, some add a trailing font-tag closer). Older
// Phantom/Inspire-generation firmware instead embeds a single
// "GPS(longitude,latitude,altitude)" triple (that ordering -- longitude
// first -- confirmed against a third-party DJI-SRT-to-GPX converter written
// specifically to consume it). This parser accepts either, and is
// deliberately tolerant of whitespace/case variance around the bracketed
// keys (a space before the colon, e.g. "[latitude : ...]", has been observed
// on some firmware) -- but does not attempt to enumerate every DJI model's
// exact field list, since it only needs the first fix.
package djisrt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

// Point is a single geotagged fix extracted from a .srt sidecar -- one GPS
// point, not a flight path. Latitude/Longitude are signed decimal degrees,
// the same convention as branchDAM's probe.ExifResult.GPSLatitude/
// GPSLongitude (hemisphere-corrected, not an unsigned EXIF magnitude+ref
// pair) and this repo's NodeCreatedPayload.GPSLatitude/GPSLongitude.
type Point struct {
	Latitude  float64
	Longitude float64
}

// ErrNoGPSData is returned when no cue in the file carries a GPS point
// recognizable by either supported format (see package doc).
var ErrNoGPSData = errors.New("djisrt: no GPS coordinates found in file")

// latBracketRe/lonBracketRe match the current-generation bracketed
// key-value format, e.g. "[latitude: 30.123456]". Case-insensitive and
// tolerant of a space before the colon -- see package doc.
var (
	latBracketRe = regexp.MustCompile(`(?i)\[\s*latitude\s*:\s*(-?\d+(?:\.\d+)?)\s*\]`)
	lonBracketRe = regexp.MustCompile(`(?i)\[\s*longitude\s*:\s*(-?\d+(?:\.\d+)?)\s*\]`)

	// gpsParenRe matches the older "GPS(longitude,latitude,altitude)"
	// inline format -- note the longitude-first ordering, see package doc.
	gpsParenRe = regexp.MustCompile(`(?i)GPS\(\s*(-?\d+(?:\.\d+)?)\s*,\s*(-?\d+(?:\.\d+)?)\s*,\s*-?\d+(?:\.\d+)?\s*\)`)
)

// ParseFirstPoint reads a DJI .srt sidecar from r and returns the first GPS
// point found in file order. Returns ErrNoGPSData if the file parses (or at
// least reads) fine but no recognizable coordinate ever appears -- a
// distinct, non-fatal outcome from a read error, since a malformed or
// GPS-less .srt is an expected input, not a bug.
//
// r is read line-by-line rather than as one parsed SRT cue at a time: DJI's
// telemetry line is not standard subtitle prose, so there is no benefit to
// a real SRT cue parser here -- a single-pass regex search of the raw text
// is both simpler and more tolerant of the block-formatting variance
// (blank-line placement, a trailing <font> closing tag on its own line,
// CRLF vs LF) observed across DJI models/firmware. This tolerance is
// specifically about surrounding formatting, not field placement: both
// [latitude: ...] and [longitude: ...] (or both halves of GPS(...,...))
// must appear on the SAME line/cue to be matched together -- every sample
// this package's format research turned up keeps them on one line, but a
// firmware variant that splits them across lines within one cue would not
// be recognized (ParseFirstPoint keeps scanning and eventually returns
// ErrNoGPSData rather than combining a partial match across lines).
func ParseFirstPoint(r io.Reader) (Point, error) {
	scanner := bufio.NewScanner(r)
	// DJI's real per-cue payload line is short, but some firmware wraps the
	// whole telemetry line (all bracketed fields together) well past
	// bufio.Scanner's 64KiB default -- give it generous headroom rather than
	// letting a long line silently fail the scan.
	buf := make([]byte, 0, 4096)
	scanner.Buffer(buf, 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if p, ok := extractPoint(line); ok {
			return p, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return Point{}, fmt.Errorf("djisrt: read: %w", err)
	}
	return Point{}, ErrNoGPSData
}

// extractPoint tries both supported formats against a single line/cue of
// text and reports whether it found a usable point. A syntactically parsed
// coordinate is still rejected (ok=false, ParseFirstPoint keeps scanning) if
// it fails isValidFix -- see that function's doc comment.
func extractPoint(text string) (Point, bool) {
	latM := latBracketRe.FindStringSubmatch(text)
	lonM := lonBracketRe.FindStringSubmatch(text)
	if latM != nil && lonM != nil {
		lat, errLat := strconv.ParseFloat(latM[1], 64)
		lon, errLon := strconv.ParseFloat(lonM[1], 64)
		if errLat == nil && errLon == nil && isValidFix(lat, lon) {
			return Point{Latitude: lat, Longitude: lon}, true
		}
	}

	if m := gpsParenRe.FindStringSubmatch(text); m != nil {
		lon, errLon := strconv.ParseFloat(m[1], 64)
		lat, errLat := strconv.ParseFloat(m[2], 64)
		if errLat == nil && errLon == nil && isValidFix(lat, lon) {
			return Point{Latitude: lat, Longitude: lon}, true
		}
	}

	return Point{}, false
}

// isValidFix rejects two classes of syntactically-parsed-but-unusable
// coordinates:
//
//  1. Out of range for signed decimal degrees (-90..90 latitude, -180..180
//     longitude) -- a value like "latitude: 3012.5" is not a real fix, just
//     a field this parser's tolerant regex happened to match.
//  2. Exactly (0, 0) -- DJI firmware commonly emits leading cues before GPS
//     lock with a (0,0) placeholder fix while the aircraft is still
//     acquiring satellites. Since ParseFirstPoint returns the FIRST match
//     (correct per this package's single-point contract), treating (0,0) as
//     real would silently return a bogus pre-lock placeholder instead of
//     the aircraft's actual first fix later in the same file. A genuine
//     (0,0) fix (Gulf of Guinea) is not a plausible drone flight location,
//     so this heuristic costs nothing in practice.
func isValidFix(lat, lon float64) bool {
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return false
	}
	if lat == 0 && lon == 0 {
		return false
	}
	return true
}
