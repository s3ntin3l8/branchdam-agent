package tray

// updatePhaseOwnsBinary reports phases during which an apply has acquired
// the idle gate and may be mutating the installed files. A read-only check is
// intentionally excluded: rollback remains safe and accurately labelled
// while GitHub metadata is being queried.
func updatePhaseOwnsBinary(phase string) bool {
	switch phase {
	case UpdatePhaseDownloading, UpdatePhaseVerifying, UpdatePhaseRestarting:
		return true
	default:
		return false
	}
}

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
