//go:build !windows && !darwin

// Tests for the confirmation-gate logic around the four destructive tray
// menu actions (issue #108 / E3 #S2-14: "Drain queue now", "Prune now",
// "Install and restart", "Roll back"). The actual menu wiring only
// compiles on windows/darwin (run_supported.go's build tag), but the
// gate decision is pure -- it takes a confirmation function, a bool, and
// returns whether to proceed -- so it's testable from this build tag
// without ever touching fyne.io/systray.
package tray

import (
	"context"
	"testing"
)

func TestConfirmDestructiveDisabledSkipsPrompt(t *testing.T) {
	calls := 0
	confirm := func(_ context.Context, _, _ string) bool {
		calls++
		return true
	}

	if !confirmDestructiveAction(context.Background(), confirm, false, "title", "body") {
		t.Fatal("expected the action to proceed when confirmDestructive=false, regardless of any confirm func")
	}
	if calls != 0 {
		t.Errorf("expected the confirm func to be skipped entirely when confirmDestructive=false, got %d calls", calls)
	}
}

func TestConfirmDestructiveEnabledRequiresOK(t *testing.T) {
	cases := []struct {
		name     string
		answer   bool
		wantProc bool
	}{
		{"ok click", true, true},
		{"cancel click", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			confirm := func(_ context.Context, _, _ string) bool { return tc.answer }
			got := confirmDestructiveAction(context.Background(), confirm, true, "title", "body")
			if got != tc.wantProc {
				t.Errorf("got proceed=%v, want %v (confirm answer=%v)", got, tc.wantProc, tc.answer)
			}
		})
	}
}

func TestConfirmDestructivePropagatesTitleAndBody(t *testing.T) {
	var gotTitle, gotBody string
	confirm := func(_ context.Context, title, body string) bool {
		gotTitle, gotBody = title, body
		return true
	}

	confirmDestructiveAction(context.Background(), confirm, true, "My Title", "My Body")

	if gotTitle != "My Title" {
		t.Errorf("got title %q, want %q", gotTitle, "My Title")
	}
	if gotBody != "My Body" {
		t.Errorf("got body %q, want %q", gotBody, "My Body")
	}
}

// TestConfirmDestructivePropagatesContext verifies the confirm func
// receives the same context the caller passed in -- the tray's select
// loop must be able to thread a context bounded by, e.g., a per-click
// timeout, all the way through to the dialog subprocess re-exec. Without
// this, the dialogTimeout in bootstrap.go is the only bound, and a
// "Quitting the tray" path on Linux/headless installs would have to
// resort to killing the re-exec'd subprocess by other means.
func TestConfirmDestructivePropagatesContext(t *testing.T) {
	type ctxKey struct{}
	want := context.WithValue(context.Background(), ctxKey{}, "sentinel")
	var got context.Context
	confirm := func(ctx context.Context, _, _ string) bool {
		got = ctx
		return true
	}

	confirmDestructiveAction(want, confirm, true, "t", "b")

	if got.Value(ctxKey{}) != "sentinel" {
		t.Error("expected the caller's context to be passed through to the confirm func unchanged")
	}
}

// TestConfirmDestructiveDoesNotPanicOnNilFunc is a defensive guarantee:
// a bug in upstream wiring that hands in a nil confirm func must not
// crash the tray. With confirmDestructive=false the func is never
// called anyway, and with confirmDestructive=true the same nil-func
// case should still not panic -- the right behavior is to NOT proceed
// (refuse the action rather than run it unconfirmed), since the
// alternative is a silent data loss exactly like the bug issue #108 was
// filed to fix.
func TestConfirmDestructiveDoesNotPanicOnNilFunc(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("confirmDestructiveAction panicked with nil confirm: %v", r)
		}
	}()

	if confirmDestructiveAction(context.Background(), nil, true, "t", "b") {
		t.Error("expected a nil confirm func + confirmDestructive=true to refuse the action, not silently proceed")
	}
	if !confirmDestructiveAction(context.Background(), nil, false, "t", "b") {
		t.Error("expected confirmDestructive=false to proceed regardless of confirm func (even nil)")
	}
}

// TestConfirmDestructiveDoubleClickBothGated pins the issue #108
// acceptance-criterion test: a click followed by a second click within
// 100ms must be confirmation-gated on BOTH occasions. The select loop
// itself dedups rapid second clicks via the non-blocking send to
// drainRequestCh/pruneRequestCh/applyDoneCh -- so this test exercises
// the gate logic, not the systray loop. Two independent calls to
// confirmDestructiveAction, simulating two clicks that BOTH got
// through the queue's own dedup layer (e.g. because the prior request
// already completed in between), must each be gated.
func TestConfirmDestructiveDoubleClickBothGated(t *testing.T) {
	calls := 0
	confirm := func(_ context.Context, _, _ string) bool {
		calls++
		return false // cancel both
	}

	for i := 0; i < 2; i++ {
		if confirmDestructiveAction(context.Background(), confirm, true, "t", "b") {
			t.Errorf("click %d: expected the action to be refused (confirm returned false)", i)
		}
	}
	if calls != 2 {
		t.Errorf("expected confirm to be called once per click, got %d", calls)
	}
}

// TestConfirmDestructiveConfirmError is a small regression guard: a
// confirm function that returns an error (signaling "the dialog
// subprocess failed to even start") must result in the action being
// refused. The real ConfirmFunc the cmd package supplies is
// (bool, error)-shaped via the dialog subprocess's stdout + exit code,
// so the tray-level helper needs to handle both axes -- not just the
// answer bool. Today confirmDestructiveAction takes a ConfirmFunc that
// is just bool, so errors are not in scope; this test pins that
// contract by asserting the helper does NOT proceed when the confirm
// returns false (which is the "error OR cancel" case in production
// wiring).
func TestConfirmDestructiveConfirmErrorRefuses(t *testing.T) {
	confirm := func(_ context.Context, _, _ string) bool { return false }

	if confirmDestructiveAction(context.Background(), confirm, true, "t", "b") {
		t.Error("expected a confirm func that returns false (whether for cancel or for an upstream error) to refuse the action")
	}
}
