package selfupdate

import "fmt"

// CheckResult is the outcome of a Check call. Its String() is what
// `branchdam-agent update`'s CLI output renders directly
// (cmd/branchdam-agent/update.go); internal/tray.UpdateStatus.Note()
// computes an equivalent but independent string from its own fields
// rather than embedding or calling into a CheckResult.
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
