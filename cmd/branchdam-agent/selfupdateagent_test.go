package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/selfupdate"
	"github.com/s3ntin3l8/branchdam-agent/internal/tray"
)

func TestRecordApplyErrorPersistsPhaseAndError(t *testing.T) {
	a := &selfUpdateAgent{enabled: true, st: tray.UpdateStatus{Enabled: true, UpdateFound: true}}
	want := errors.New("download stalled")
	a.recordApplyError(want)

	got := a.Status()
	if got.Phase != updatePhaseFailed {
		t.Errorf("Phase = %q, want %q", got.Phase, updatePhaseFailed)
	}
	if !errors.Is(got.Err, want) {
		t.Errorf("Err = %v, want %v", got.Err, want)
	}
}

func TestTrayUpdateApplierRejectsApplyDuringCheck(t *testing.T) {
	updater := &selfUpdateAgent{
		enabled: true,
		st: tray.UpdateStatus{
			Enabled:     true,
			UpdateFound: true,
			Phase:       updatePhaseChecking,
		},
	}
	applier := &trayUpdateApplier{updater: updater}
	status, started := applier.StartApply()
	if started {
		t.Fatal("started = true, want false while the update check is running")
	}
	if status.Err == nil || status.Err.Error() != "self-update: check/apply is already checking" {
		t.Fatalf("status.Err = %v, want check-in-progress error", status.Err)
	}
}

func TestTrayUpdateApplierAllowsRetryAfterCheckFailure(t *testing.T) {
	runner := tray.NewRunner(nil, nil, "")
	release, ok := runner.TryLockIdle()
	if !ok {
		t.Fatal("could not reserve runner gate")
	}
	defer release()

	updater := &selfUpdateAgent{
		enabled: true,
		st: tray.UpdateStatus{
			Enabled:     true,
			UpdateFound: true,
			Phase:       updatePhaseFailed,
			// A failed check has no StartedAt, but it must not make a
			// still-available update permanently un-installable.
		},
	}
	applier := &trayUpdateApplier{runner: runner, updater: updater}
	status, started := applier.StartApply()
	if started {
		t.Fatal("started = true, want false while the runner gate is busy")
	}
	if status.Err == nil || status.Err.Error() != "self-update: an ingest or update is already in progress" {
		t.Fatalf("status.Err = %v, want runner-busy error (failed phase must be retryable)", status.Err)
	}
}

func TestTrayUpdateApplierRejectsApplyWhileApplyLocked(t *testing.T) {
	updater := &selfUpdateAgent{
		enabled: true,
		st:      tray.UpdateStatus{Enabled: true, UpdateFound: true, Phase: updatePhaseAvailable},
	}
	updater.applyMu.Lock()
	defer updater.applyMu.Unlock()

	applier := &trayUpdateApplier{runner: tray.NewRunner(nil, nil, ""), updater: updater}
	status, started := applier.StartApply()
	if started {
		t.Fatal("started = true, want false while applyMu is reserved")
	}
	if status.Err == nil || status.Err.Error() != "self-update: an update or check is already in progress" {
		t.Fatalf("status.Err = %v, want apply-lock error", status.Err)
	}
}

func TestTrayUpdateApplierReservesAndReleasesLocks(t *testing.T) {
	runner := tray.NewRunner(nil, nil, "")
	updater := &selfUpdateAgent{
		enabled: true,
		st:      tray.UpdateStatus{Enabled: true, UpdateFound: true, Phase: updatePhaseAvailable},
		// A nil updater makes the worker fail immediately after it has
		// reserved both locks, keeping this test offline and deterministic.
	}
	applier := &trayUpdateApplier{runner: runner, updater: updater}
	status, started := applier.StartApply()
	if !started {
		t.Fatalf("started = false, status = %+v", status)
	}
	if status.Phase != updatePhaseDownloading {
		t.Errorf("status.Phase = %q, want %q before worker starts", status.Phase, updatePhaseDownloading)
	}

	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for {
		if got := updater.Status(); got.Phase == updatePhaseFailed && updater.applyMu.TryLock() {
			if release, ok := runner.TryLockIdle(); ok {
				release()
				updater.applyMu.Unlock()
				return
			}
			updater.applyMu.Unlock()
		}
		select {
		case <-deadline.C:
			t.Fatalf("worker did not record its failure: status = %+v", updater.Status())
		case <-time.After(time.Millisecond):
		}
	}
}

// TestSelfUpdateAgentRollbackAvailableFalseByDefault and
// TestSelfUpdateAgentRollbackFailsWithoutPrevious both rely on the real
// go test binary naturally having no ".previous"/".previous.version"
// sidecar next to it -- RollbackAvailable/Rollback resolve via
// os.Executable(), which can't be faked in-process, so these only
// exercise the "no rollback available" path. The full restore-from-
// backup logic is covered by internal/selfupdate's own rollback_test.go
// against fabricated InstallLayouts.

func TestSelfUpdateAgentRollbackAvailableFalseByDefault(t *testing.T) {
	a := &selfUpdateAgent{}
	if version, ok := a.RollbackAvailable(); ok {
		t.Errorf("expected RollbackAvailable=false, got version=%q ok=true", version)
	}
}

func TestSelfUpdateAgentRollbackFailsWithoutPrevious(t *testing.T) {
	a := &selfUpdateAgent{}
	if _, err := a.Rollback(context.Background()); err == nil {
		t.Error("expected Rollback to return an error when no previous version is available")
	}
}

// TestCheckNowDisabledDoesNotPanic guards the exact crash CheckNow's own
// doc comment warns about: check() dereferences a.up with no nil check of
// its own (Run's callers already guard that before ever calling
// checkOnce), so CheckNow's !a.enabled || a.up == nil guard must run
// BEFORE checkOnce/check ever sees a nil a.up, or a disabled agent's
// manual check would panic inside an HTTP handler goroutine.
func TestCheckNowDisabledDoesNotPanic(t *testing.T) {
	a := &selfUpdateAgent{enabled: false} // a.up is nil, the zero value
	status, ran := a.CheckNow(context.Background())
	if ran {
		t.Errorf("ran = true, want false for a disabled agent")
	}
	if status.Enabled {
		t.Errorf("status.Enabled = true, want false")
	}
}

// TestCheckNowConstructionFailedDoesNotPanic covers the other nil-a.up
// path: enabled but selfupdate.NewUpdater failed at construction time
// (newSelfUpdateAgent's own doc comment), which also leaves a.up nil.
func TestCheckNowConstructionFailedDoesNotPanic(t *testing.T) {
	a := &selfUpdateAgent{enabled: true, up: nil}
	if _, ran := a.CheckNow(context.Background()); ran {
		t.Errorf("ran = true, want false when a.up is nil despite enabled=true")
	}
}

// TestCheckNowStoresStatusButReportsNotRunOnUnavailable confirms two
// things via the real check() path, fully offline: (1) CheckNow updates
// a.st (not just its own return value) even when ran=false, and (2) a
// non-semver build reports ran=false, matching CheckNow's own doc comment
// ("ran=false covers ... a non-semver build") -- an earlier version of
// this method folded checkOnce's `unavailable` into a true `ran`, which
// would have reported ran=true (misleadingly implying a fresh check
// result is available) for a build that can structurally never check
// successfully (Hermes review finding on this PR). a.version is
// deliberately non-semver so Check() returns ErrVersionNotSemver without
// making any network call (Updater.Check's own doc comment), letting this
// run fully offline while still exercising checkOnce's TryLock-success
// path and check()'s a.st mutation.
func TestCheckNowStoresStatusButReportsNotRunOnUnavailable(t *testing.T) {
	up, err := selfupdate.NewUpdater("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	a := &selfUpdateAgent{enabled: true, up: up, version: "not-semver", st: tray.UpdateStatus{Enabled: true}}

	status, ran := a.CheckNow(context.Background())
	if ran {
		t.Error("ran = true, want false for a non-semver build")
	}
	if !status.Unavailable || !status.Checked {
		t.Errorf("returned status = %+v, want Unavailable=true Checked=true", status)
	}
	if got := a.Status(); !got.Unavailable {
		t.Errorf("a.Status() after CheckNow = %+v, want Unavailable=true -- CheckNow must store the result even when ran=false", got)
	}
	if got := a.Status(); got.Phase != updatePhaseIdle {
		t.Errorf("a.Status().Phase = %q, want %q for an unavailable dev build", got.Phase, updatePhaseIdle)
	}
	if got := a.Status().Note(); got != "unavailable (not a released build)" {
		t.Errorf("a.Status().Note() = %q, want unavailable notice", got)
	}
}

// TestCheckNowSkipsWhileAnotherCheckRuns confirms a concurrent check (Run's
// own interval ticker, or a second manual click) reports ran=false rather
// than racing check() against the same a.up -- simulated by holding
// checkMu locked, so checkOnce's TryLock fails deterministically (a
// same-goroutine TryLock against an already-held sync.Mutex always fails;
// Go's Mutex tracks no owner, so this isn't a race to win, just a locked
// gate) before ever reaching check()/a.up. a.up is a real *Updater with a
// non-semver a.version, same as TestCheckNowStoresStatus above, so IF the
// TryLock guard were ever removed, checkOnce would call a.checkMu.Lock()
// against the lock this test already holds and deadlock (timing this test
// out) rather than silently passing -- it would NOT reach the network,
// since Check() returns ErrVersionNotSemver before any network call.
func TestCheckNowSkipsWhileAnotherCheckRuns(t *testing.T) {
	up, err := selfupdate.NewUpdater("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	a := &selfUpdateAgent{enabled: true, up: up, version: "not-semver"}
	a.checkMu.Lock()
	defer a.checkMu.Unlock()

	if _, ran := a.CheckNow(context.Background()); ran {
		t.Error("ran = true, want false while another check holds checkMu")
	}
}

func TestCheckNowSkipsWhileApplyRuns(t *testing.T) {
	up, err := selfupdate.NewUpdater("owner/repo")
	if err != nil {
		t.Fatal(err)
	}
	a := &selfUpdateAgent{enabled: true, up: up, version: "not-semver"}
	a.applyMu.Lock()
	defer a.applyMu.Unlock()

	if _, ran := a.CheckNow(context.Background()); ran {
		t.Error("ran = true, want false while an apply holds applyMu")
	}
}
