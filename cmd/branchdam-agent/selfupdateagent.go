package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/selfupdate"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

// selfUpdateAgent is the tray's tray.SelfUpdater: an initial check at
// startup plus a periodic re-check (selfUpdate.checkIntervalHours),
// storing the structured selfupdate.CheckResult rather than collapsing it
// to a string the way this file's predecessor (startSelfUpdateCheck) did.
type selfUpdateAgent struct {
	enabled  bool
	version  string
	interval time.Duration

	up *selfupdate.Updater // nil when disabled or construction failed

	mu sync.Mutex
	st tray.UpdateStatus
}

// newSelfUpdateAgent builds an agent from cfg -- Run and ApplyLatest are
// no-ops when cfg.SelfUpdate.Enabled is false, so the tray/status page
// still have something truthful to show ("disabled") without ever
// contacting GitHub.
func newSelfUpdateAgent(cfg config.Config, version string) *selfUpdateAgent {
	a := &selfUpdateAgent{
		enabled:  cfg.SelfUpdate.Enabled,
		version:  version,
		interval: time.Duration(cfg.SelfUpdate.CheckIntervalHoursOrDefault()) * time.Hour,
		st:       tray.UpdateStatus{Enabled: cfg.SelfUpdate.Enabled},
	}
	if !a.enabled {
		return a
	}

	up, err := selfupdate.NewUpdater(cfg.SelfUpdate.RepoOrDefault())
	if err != nil {
		a.st.Err = err
		return a
	}
	a.up = up
	return a
}

// Status returns the current snapshot for the tray menu and status page.
func (a *selfUpdateAgent) Status() tray.UpdateStatus {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.st
}

// Run performs the initial check and, unless CheckIntervalHours disables
// it, re-checks on a ticker until ctx is cancelled. Blocks; callers run it
// in its own goroutine and join it via ctx cancellation, not a
// WaitGroup -- there is nothing to clean up on exit.
func (a *selfUpdateAgent) Run(ctx context.Context) {
	if !a.enabled || a.up == nil {
		return
	}

	if a.check(ctx) {
		return
	}
	if a.interval <= 0 {
		return
	}

	ticker := time.NewTicker(a.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if a.check(ctx) {
				return
			}
		}
	}
}

// check runs one Check call and stores the result. Returns true when the
// running version is not semver (ErrVersionNotSemver) -- re-checking can
// never succeed for such a build, so the caller stops ticking rather than
// hitting GitHub on every interval forever.
func (a *selfUpdateAgent) check(ctx context.Context) (unavailable bool) {
	result, err := a.up.Check(ctx, a.version)

	a.mu.Lock()
	defer a.mu.Unlock()
	a.st.Checked = true
	a.st.CheckedAt = time.Now()
	if err != nil {
		if errors.Is(err, selfupdate.ErrVersionNotSemver) {
			a.st.Unavailable = true
			return true
		}
		a.st.Err = err
		return false
	}
	a.st.Err = nil
	a.st.CurrentVersion = result.CurrentVersion
	a.st.LatestVersion = result.LatestVersion
	a.st.UpdateFound = result.UpdateFound
	return false
}

// RollbackAvailable reports whether a previously applied version can be
// restored right now -- a cheap local filesystem check
// (selfupdate.RollbackInfo, one combined call so a hot menu-refresh tick
// never stats the backup and reads the version sidecar twice), never a
// network call. Deliberately NOT gated on a.enabled: Rollback itself
// never contacts GitHub (it restores from the ".previous" backup a prior
// Apply left on disk), so disabling self-update checking shouldn't also
// hide an operator's ability to undo an update they already applied
// while it was enabled.
func (a *selfUpdateAgent) RollbackAvailable() (string, bool) {
	execPath, err := os.Executable()
	if err != nil {
		return "", false
	}
	layout, err := selfupdate.DetectLayout(execPath)
	if err != nil {
		return "", false
	}
	return selfupdate.RollbackInfo(layout)
}

// Rollback restores the previously applied version via
// selfupdate.Rollback and records it in Status() on success, the same
// way ApplyLatest does for a forward update. See RollbackAvailable's doc
// comment for why this isn't gated on a.enabled.
func (a *selfUpdateAgent) Rollback(_ context.Context) (string, error) {
	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("self-update: resolve own executable: %w", err)
	}
	layout, err := selfupdate.DetectLayout(execPath)
	if err != nil {
		return "", err
	}

	version, err := selfupdate.Rollback(layout)
	if err != nil {
		return "", err
	}

	a.mu.Lock()
	a.st.Applied = version
	a.mu.Unlock()
	return version, nil
}

// ApplyLatest downloads and applies the latest release to this
// installation's InstallLayout (see internal/selfupdate.DetectLayout) and
// records the applied version in Status() on success.
func (a *selfUpdateAgent) ApplyLatest(ctx context.Context) (string, error) {
	if !a.enabled || a.up == nil {
		return "", errors.New("self-update: not enabled")
	}

	execPath, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("self-update: resolve own executable: %w", err)
	}
	layout, err := selfupdate.DetectLayout(execPath)
	if err != nil {
		return "", err
	}

	appliedVersion, err := a.up.Apply(ctx, a.version, layout)
	if err != nil {
		return "", err
	}

	a.mu.Lock()
	a.st.Applied = appliedVersion
	a.mu.Unlock()
	return appliedVersion, nil
}
