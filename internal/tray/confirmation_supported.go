//go:build windows || darwin

// The confirmDestructiveAction helper is platform-independent (no
// fyne.io/systray import, no dialog backend); the windows/darwin copy
// lives here so a build on the tray's two supported platforms always
// has a real definition to compile, while !windows && !darwin builds
// (the test-only path) get the confirmation_test.go-covered copy in
// confirmation.go. The two files are duplicated rather than shared
// across build tags because the helper is the entire reason for the
// split: a function with one job ("decide whether to proceed") stays
// small enough to read in one screen, and the duplication means a
// future change to either side can never accidentally turn the other
// into a no-op.
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
