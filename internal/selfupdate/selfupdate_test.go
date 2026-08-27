package selfupdate

import (
	"context"
	"errors"
	"testing"
)

func TestCheckResultString(t *testing.T) {
	upToDate := CheckResult{CurrentVersion: "1.2.0"}
	if got := upToDate.String(); got != "up to date (1.2.0)" {
		t.Errorf("got %q", got)
	}

	updateAvailable := CheckResult{CurrentVersion: "1.2.0", LatestVersion: "1.3.0", UpdateFound: true}
	if got := updateAvailable.String(); got != "update available: 1.2.0 -> 1.3.0" {
		t.Errorf("got %q", got)
	}
}

// TestCheckNonSemverDoesNotPanic pins the fix for a live crash:
// go-selfupdate's Release.GreaterThan calls semver.MustParse internally,
// which panics on a non-semver argument -- "dev", the default main.version
// takes for a plain `go build`/`make build`, and still the default even
// for `make build-windows`/`build-darwin-app` unless VERSION=<semver> is
// passed explicitly. Check must reject that before ever reaching
// go-selfupdate, and it must do so without making a network call (this
// test never contacts GitHub -- if it did, it would hang or fail in a
// sandboxed CI run rather than exercising the gate).
func TestCheckNonSemverDoesNotPanic(t *testing.T) {
	u, err := NewUpdater("s3ntin3l8/branchdam-agent")
	if err != nil {
		t.Fatal(err)
	}

	for _, v := range []string{"dev", "", "manual-test"} {
		_, err := u.Check(context.Background(), v)
		if !errors.Is(err, ErrVersionNotSemver) {
			t.Errorf("Check(%q) err = %v, want ErrVersionNotSemver", v, err)
		}
	}
}

func TestApplyNonSemverRefusesBeforeNetwork(t *testing.T) {
	u, err := NewUpdater("s3ntin3l8/branchdam-agent")
	if err != nil {
		t.Fatal(err)
	}

	_, err = u.Apply(context.Background(), "dev", InstallLayout{Primary: "/nonexistent/branchdam-agent"})
	if !errors.Is(err, ErrVersionNotSemver) {
		t.Errorf("Apply err = %v, want ErrVersionNotSemver", err)
	}
}
