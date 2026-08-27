//go:build !windows && !darwin

// Stub for every platform other than windows/darwin -- see
// run_supported.go's doc comment for why the real implementation is
// gated the way it is. Linux CI (the repo's required test-go/lint-and-test
// check) builds and tests this file, never the systray-backed one, so
// `go build ./...`/`go vet ./...`/`go test ./...` never need a tray
// backend on the runner. Run itself is exercised by
// TestRunUnsupportedReturnsError.
package tray

import (
	"context"
	"errors"

	"github.com/s3ntin3l8/branchdam-agent/internal/ingest"
)

// ErrUnsupported is returned by Run on any platform other than
// windows/darwin. The tray is scoped to those two per the plan doc and
// issue #3; a Linux workstation still has the fully-tested headless
// `ingest`/`ingest --watch` path, just no tray icon.
var ErrUnsupported = errors.New("tray: unsupported on this platform (windows and darwin only); use `branchdam-agent ingest` instead")

// Run always returns ErrUnsupported on this platform.
func Run(_ context.Context, _ *Runner, _ *ingest.Detector, _ string, _ SelfUpdater) (Outcome, error) {
	return Outcome{}, ErrUnsupported
}
