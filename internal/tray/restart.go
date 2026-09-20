package tray

// windowApplyRestartOutcome converts a completed native-window apply into
// the same restart request the tray menu's apply handler returns. Kept pure so
// the platform-tagged systray loop can use it without making the decision
// depend on detector callback goroutines or desktop APIs.
func windowApplyRestartOutcome(status UpdateStatus, trayApplying, rollingBack bool) (Outcome, bool) {
	if status.Phase != UpdatePhaseRestarting || trayApplying || rollingBack {
		return Outcome{}, false
	}
	return Outcome{RestartRequested: true, AppliedVersion: status.Applied}, true
}
