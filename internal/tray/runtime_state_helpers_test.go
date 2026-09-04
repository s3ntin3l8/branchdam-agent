package tray

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	internruntime "github.com/s3ntin3l8/branchdam-agent/internal/runtime"
)

// osWriteJSON writes a runtime-shaped JSON file at path containing a
// single LastHandshakeAt field. Test-only helper used by
// TestCrossRestartLastHandshakeAtSurvivesViaRuntimeFile to simulate
// the prior session's last successful drain without depending on
// internal/runtime directly from the test body (the test is pinning
// the cross-restart shape; the runtime package's own
// round-trip-load test pins its own internal contract).
func osWriteJSON(path string, when time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		LastHandshakeAt time.Time `json:"lastHandshakeAt,omitempty"`
	}{LastHandshakeAt: when})
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o600)
}

// loadRuntimeStateForTest is the test-side mirror of runTrayCmd's
// "Load + extract LastHandshakeAt" sequence: it imports
// internal/runtime for the Load function (production wiring
// explicitly does this in cmd/branchdam-agent/tray.go) and returns
// the LastHandshakeAt field as a time.Time. The tray internal itself
// takes the time.Time via SeedLastHandshakeAt; keeping the Load
// call at the wiring layer (cmd/branchdam-agent) rather than inside
// internal/tray is what preserves the "Runner is interface-only,
// not a file-IO package" boundary.
func loadRuntimeStateForTest(path string) (time.Time, error) {
	st, err := internruntime.Load(path)
	if err != nil {
		return time.Time{}, err
	}
	return st.LastHandshakeAt, nil
}
