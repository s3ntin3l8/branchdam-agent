//go:build !windows && !darwin

// The confirmDestructiveAction helper is platform-independent (no
// fyne.io/systray import, no dialog backend) so the destructive-action
// gate logic can be unit-tested on any host, including the Linux CI
// runner -- see confirmation_test.go. The run_supported.go select loop
// calls confirmDestructiveAction from each of the four destructive
// click handlers (issue #108 / E3 #S2-14: "Drain queue now",
// "Prune now", "Install and restart", "Roll back"). This file is
// built only on !windows && !darwin, where confirmation_test.go lives;
// the windows/darwin copy is confirmation_supported.go, so a future
// change to either side can never accidentally turn the other into a
// no-op.
package tray

import "context"

// confirmDestructiveAction is the gate around the four destructive
// tray menu actions (issue #108 / E3 #S2-14: "Drain queue now",
// "Prune now", "Install and restart", "Roll back"). It returns true
// iff the action should proceed:
//   - When enabled is false (the operator opted out via
//     tray.confirmDestructive: false), always true -- the confirm func
//     is not even called, so a nil confirm func is safe to pass.
//   - When enabled is true, the action proceeds iff confirm(title,
//     body) returns true. A false return -- whether the operator
//     clicked Cancel, the dialog subprocess failed, or the confirm
//     func itself is nil (defensive, see the test) -- refuses the
//     action: the alternative is a silent data loss exactly like the
//     bug the issue was filed to fix.
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
	return confirm(ctx, title, body)
}
