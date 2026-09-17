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

	// checkMu serializes check() calls -- Run's own interval ticker and a
	// manual CheckNow (the Settings window's "Check for updates" button)
	// can both fire around the same time, and both ultimately hit the
	// same GitHub API; checkOnce's TryLock makes a concurrent call a
	// harmless no-op (ran=false) rather than two overlapping requests.
	checkMu sync.Mutex
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

	if unavailable, _ := a.checkOnce(ctx); unavailable {
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
			if unavailable, _ := a.checkOnce(ctx); unavailable {
				return
			}
		}
	}
}

// checkOnce runs check under checkMu, so Run's own ticker and a manual
// CheckNow (below) can never run check concurrently against the same
// GitHub API. ran is false when another check is already in flight --
// callers must treat that as "nothing happened," not as unavailable or an
// error; TryLock failing has nothing to do with the running build being
// non-semver.
func (a *selfUpdateAgent) checkOnce(ctx context.Context) (unavailable, ran bool) {
	if !a.checkMu.TryLock() {
		return false, false
	}
	defer a.checkMu.Unlock()
	return a.check(ctx), true
}

// check runs one Check call and stores the result. Returns true when the
// running version is not semver (ErrVersionNotSemver) -- re-checking can
// never succeed for such a build, so the caller stops ticking rather than
// hitting GitHub on every interval forever. Callers other than checkOnce
// must not call this directly -- it does not itself guard against
// concurrent use of a.up.
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

// CheckNow implements tray.UpdateChecker for the Settings window's
// on-demand "Check for updates" button. The !a.enabled || a.up == nil
// guard runs BEFORE ever reaching checkOnce/check: check() dereferences
// a.up with no nil check of its own (Run's own callers above already
// guard that before ever calling checkOnce), so skipping this guard would
// let a disabled agent's -- or a construction-failure's -- manual check
// panic inside an HTTP handler goroutine. ran=false covers three non-error
// outcomes a caller must not surface as a failure: disabled, a non-semver
// build, and a check already in flight (Run's ticker firing at the same
// moment).
func (a *selfUpdateAgent) CheckNow(ctx context.Context) (tray.UpdateStatus, bool) {
	if !a.enabled || a.up == nil {
		return a.Status(), false
	}
	unavailable, ran := a.checkOnce(ctx)
	if !ran || unavailable {
		// unavailable (non-semver build) is folded into ran=false here,
		// matching this method's own doc comment -- without this, a click
		// in the seconds before the startup check first lands would
		// report ran=true with Status().Unavailable=true, which the
		// frontend's own contract ("ran=false means nothing changed")
		// never anticipates (Hermes review finding on this PR). Latent in
		// practice: the button is already disabled once su.Unavailable is
		// known, but that's the frontend's belt, not this method's own
		// correctness.
		return a.Status(), false
	}
	return a.Status(), true
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
