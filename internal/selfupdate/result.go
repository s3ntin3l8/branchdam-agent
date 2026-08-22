package selfupdate

import "fmt"

// CheckResult is the outcome of a Check call -- what
// internal/tray.Status.SelfUpdateNote renders. Deliberately untagged (no
// //go:build selfupdate) so it -- and TestCheckResultString -- compile and
// run identically whether or not the real implementation
// (selfupdate.go, //go:build selfupdate) or the stub
// (selfupdate_stub.go, //go:build !selfupdate) is what's actually linked
// in. See selfupdate.go's doc comment for why this package is split this
// way at all.
type CheckResult struct {
	CurrentVersion string
	LatestVersion  string
	UpdateFound    bool
}

func (r CheckResult) String() string {
	if !r.UpdateFound {
		return fmt.Sprintf("up to date (%s)", r.CurrentVersion)
	}
	return fmt.Sprintf("update available: %s -> %s", r.CurrentVersion, r.LatestVersion)
}
