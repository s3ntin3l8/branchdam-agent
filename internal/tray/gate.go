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
)

// confirmDestructiveAction is the gate around the four destructive
// tray menu actions. It returns true iff the action should proceed:
//   - When enabled is false (the operator opted out via
//     tray.confirmDestructive: false), always true -- the confirm func
//     is not even called, so a nil confirm func is safe to pass.
//   - When enabled is true, the action proceeds iff confirm(title,
//     body) returns true. A false return --
//     whether the operator clicked Cancel, the dialog subprocess
//     failed, or the confirm func itself is nil (defensive, see the
//     test) -- refuses the action: the alternative is a silent data
//     loss exactly like the bug the issue was filed to fix.
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
