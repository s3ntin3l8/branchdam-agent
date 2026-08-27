package selfupdate

import "fmt"

// CheckResult is the outcome of a Check call -- what
// internal/tray.UpdateStatus.Note renders.
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
