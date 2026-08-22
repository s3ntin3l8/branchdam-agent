//go:build !selfupdate

// Stub compiled into every DEFAULT build of this repo (make build, make
// check, and this repo's required CI). See selfupdate.go's doc comment
// (that file only compiles in with an explicit `-tags selfupdate` build
// flag) for the govulncheck finding that forces this split. Both Check and
// Apply return ErrNotCompiledIn rather than doing anything -- if an
// operator sets selfUpdate.enabled: true in config.yaml against a
// default-built binary, that error string is exactly what
// startSelfUpdateCheck (cmd/branchdam-agent/tray.go) surfaces on the tray's
// status page, which is the correct, truthful behavior: the config flag
// alone is not sufficient in a default build, and the operator needs to
// know that rather than see a silent no-op.
package selfupdate

import (
	"context"
	"errors"
)

// ErrNotCompiledIn is returned by Check and Apply in a default build of
// this repo (i.e. one built without `-tags selfupdate`).
var ErrNotCompiledIn = errors.New("selfupdate: not compiled into this build (rebuild with -tags selfupdate)")

// Check always returns ErrNotCompiledIn in a default build.
func Check(_ context.Context, _ string, currentVersion string) (CheckResult, error) {
	return CheckResult{CurrentVersion: currentVersion}, ErrNotCompiledIn
}

// Apply always returns ErrNotCompiledIn in a default build.
func Apply(_ context.Context, _ string, _ string, _ string) (string, error) {
	return "", ErrNotCompiledIn
}
