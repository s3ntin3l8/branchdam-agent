// The confirmation-gate logic around the four destructive tray menu
// actions (issue #108 / E3 #S2-14: "Drain queue now", "Prune now",
// "Install and restart", "Roll back") lives here, in a file with NO
// build tag, so the exact bytes that run in production on
// windows/darwin are the same ones confirmation_test.go exercises on
// the Linux CI runner. It was previously duplicated across a
// `!windows && !darwin` copy (tested, never shipped) and a
// `windows || darwin` copy (shipped, never tested), with nothing
// keeping the two in sync. The helper is pure -- no fyne.io/systray
// import, no dialog backend -- so no build tag is needed at all; the
// menu wiring that calls it stays in run_supported.go.
package tray

import (
	"context"
	"time"
)

// confirmTimeout bounds how long a single destructive-action click is
// allowed to wait on the operator's answer. The dialog subprocess has
// its own, much longer bound (cmd/branchdam-agent's dialogTimeout, 5
// minutes) which is fine for a subprocess but not for the tray: the
// gate is called from run_supported.go's GUI select loop, so a stalled
// dialog (headless/wonky display, frozen zenity) would otherwise wedge
// the whole tray -- Quit included -- for the full subprocess bound.
// 60s is long enough for a human who is looking at the dialog and
// short enough that a dialog that is never going to appear can't hold
// the menu hostage. A timeout refuses the action, same as Cancel.
const confirmTimeout = 60 * time.Second

// confirmDestructiveAction is the gate around the four destructive
// tray menu actions. It returns true iff the action should proceed:
//   - When enabled is false (the operator opted out via
//     tray.confirmDestructive: false), always true -- the confirm func
//     is not even called, so a nil confirm func is safe to pass.
//   - When enabled is true, the action proceeds iff confirm(title,
//     body) returns true within confirmTimeout. A false return --
//     whether the operator clicked Cancel, the dialog subprocess
//     failed, or the confirm func itself is nil (defensive, see the
//     test) -- refuses the action: the alternative is a silent data
//     loss exactly like the bug the issue was filed to fix.
//
// confirm runs on its own goroutine, under a child context bounded by
// confirmTimeout, and the gate also gives up if ctx itself is
// cancelled (tray shutdown). Either way it refuses rather than
// proceeds. The goroutine is never waited on after that point: it
// writes to a buffered channel, so a confirm that eventually returns
// long after the timeout exits cleanly instead of leaking, and its own
// ctx is already cancelled so the dialog subprocess is torn down with
// it.
//
// A nil confirm func is treated as a refusal, never a silent proceed,
// even when enabled is true. Run is the only caller; the production
// wiring always supplies a real one.
func confirmDestructiveAction(ctx context.Context, confirm func(ctx context.Context, title, body string) bool, enabled bool, title, body string) bool {
	if !enabled {
		return true
	}
	if confirm == nil {
		return false
	}

	cctx, cancel := context.WithTimeout(ctx, confirmTimeout)
	defer cancel()

	answer := make(chan bool, 1)
	go func() {
		answer <- confirm(cctx, title, body)
	}()

	select {
	case ok := <-answer:
		return ok
	case <-cctx.Done():
		// Timed out, or the tray is shutting down. Refuse: an
		// unanswered prompt is not consent.
		return false
	}
}
