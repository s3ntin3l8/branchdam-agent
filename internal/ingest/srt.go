package ingest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/djisrt"
)

// findSRTSidecar looks for a same-stem ".srt"/".SRT" sidecar next to a video
// file at videoPath -- a raw filename-stem lookup (same directory, same
// name up to the last extension, case varied), deliberately NOT run through
// internal/naming.Stem, matching the same reasoning as this package's own
// sidecarPath (an on-disk lookup, not a graph-identity comparison). DJI
// firmware has been observed to write both ".SRT" and ".srt" depending on
// model/generation, so both are tried.
func findSRTSidecar(videoPath string) (string, bool) {
	base := strings.TrimSuffix(videoPath, filepath.Ext(videoPath))
	for _, ext := range []string{".srt", ".SRT", ".Srt"} {
		candidate := base + ext
		info, err := os.Lstat(candidate)
		if err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

// srtGPS parses srtPath's first GPS fix via internal/djisrt.ParseFirstPoint
// -- the same "first fix only, reject pre-lock (0,0)" contract as
// branchDAM's own internal/djisrt (see that package's doc comment). Returns
// ok=false (not an error) when the file has no recognizable/valid fix
// (djisrt.ErrNoGPSData) -- a video's own .srt existing but carrying no GPS
// (e.g. GPS never acquired during the whole flight) is an expected outcome,
// not a failure that should abort ingest of the video itself.
func srtGPS(srtPath string) (lat, lon float64, ok bool, err error) {
	f, err := os.Open(srtPath) //nolint:gosec // srtPath resolved from the card's own file tree, not attacker input
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = f.Close() }()

	p, err := djisrt.ParseFirstPoint(f)
	if err != nil {
		if errors.Is(err, djisrt.ErrNoGPSData) {
			return 0, 0, false, nil
		}
		return 0, 0, false, err
	}
	return p.Latitude, p.Longitude, true, nil
}
