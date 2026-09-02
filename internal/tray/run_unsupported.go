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
)

// Run always returns ErrUnsupported on this platform. confirm and
// confirmDestructive are accepted but ignored on this stub -- see
// run_supported.go for the real wiring.
func Run(_ context.Context, _ *Runner, _ string, _ SelfUpdater, _ Settings, _ func(ctx context.Context, title, body string) bool, _ bool) (Outcome, error) {
	return Outcome{}, ErrUnsupported
}
