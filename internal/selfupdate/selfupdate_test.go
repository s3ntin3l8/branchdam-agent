package selfupdate

import "testing"

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
