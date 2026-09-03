// Tests for the confirmation-gate logic around the four destructive tray
// menu actions (issue #108 / E3 #S2-14: "Drain queue now", "Prune now",
// "Install and restart", "Roll back"). The actual menu wiring only
// compiles on windows/darwin (run_supported.go's build tag), but the
// gate decision itself is pure -- it takes a confirmation function, a
// bool, and returns whether to proceed -- so gate.go carries no build
// tag and neither does this file: the exact code that ships on
// windows/darwin is what runs here, on the Linux CI runner, without
// ever touching fyne.io/systray.
package tray

import (
	"context"
	"testing"
	"time"
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

// TestConfirmDestructiveConfirmErrorRefuses pins the bool-only confirm
// contract: the func the gate takes is
// `func(context.Context, string, string) bool` -- there is no error
// return, by design. The cmd package's trayConfirm collapses both axes
// of the dialog subprocess (a Cancel click AND a failure to run the
// dialog at all) into a single bool, where false means
// "cancel-or-error". This test asserts the gate honors that: false
// never proceeds, whatever produced it.
func TestConfirmDestructiveConfirmErrorRefuses(t *testing.T) {
	confirm := func(_ context.Context, _, _ string) bool { return false }

	if confirmDestructiveAction(context.Background(), confirm, true, "t", "b") {
		t.Error("expected a confirm func that returns false (whether for cancel or for an upstream error) to refuse the action")
	}
}

// TestConfirmDestructiveTimesOutRefuses covers the wedged-dialog case
// (a headless/wonky display, a frozen zenity): the gate is called from
// run_supported.go's GUI select loop, so a confirm that never returns
// must not hold the whole tray -- Quit included -- hostage. The gate
// runs confirm on its own goroutine under a confirmTimeout-bounded
// child context; when that context fires, the gate gives up and
// refuses. Rather than sleeping for the real 60s bound, this test
// drives the equivalent path through an already-short caller context.
func TestConfirmDestructiveTimesOutRefuses(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	confirm := func(ctx context.Context, _, _ string) bool {
		close(started)
		select {
		case <-ctx.Done(): // the gate must cancel us on its way out
		case <-release:
		}
		return true // an answer that arrives too late must not proceed
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	done := make(chan bool, 1)
	go func() { done <- confirmDestructiveAction(ctx, confirm, true, "t", "b") }()

	<-started
	select {
	case got := <-done:
		if got {
			t.Error("expected a confirm that never answers to refuse the action once the context is done")
		}
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("confirmDestructiveAction blocked past its context deadline -- the tray select loop would be wedged")
	}
}

// TestConfirmDestructiveCancelsConfirmContext asserts the gate hands
// confirm a context it actually cancels when it gives up, so the
// dialog subprocess the production trayConfirm re-execs is torn down
// with it rather than lingering for the full 5-minute dialogTimeout.
func TestConfirmDestructiveCancelsConfirmContext(t *testing.T) {
	cancelled := make(chan struct{})
	confirm := func(ctx context.Context, _, _ string) bool {
		<-ctx.Done()
		close(cancelled)
		return false
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	confirmDestructiveAction(ctx, confirm, true, "t", "b")

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("expected the gate to cancel the context it passed to confirm")
	}
}

// TestConfirmDestructiveTimeoutBound documents the per-click bound
// itself: it must stay well under cmd/branchdam-agent's dialogTimeout
// (5 minutes), which bounds the subprocess, not the GUI loop.
func TestConfirmDestructiveTimeoutBound(t *testing.T) {
	if confirmTimeout <= 0 || confirmTimeout >= 5*time.Minute {
		t.Errorf("confirmTimeout = %v, want a positive bound well under the 5m dialogTimeout", confirmTimeout)
	}
}
