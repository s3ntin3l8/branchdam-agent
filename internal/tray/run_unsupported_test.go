//go:build !windows && !darwin

package tray

import (
	"context"
	"errors"
	"testing"
)

func TestRunUnsupportedReturnsError(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	_, err := Run(context.Background(), r, "http://127.0.0.1:38080/", fakeSelfUpdater{}, fakeSettings{}, noopConfirm, false)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}

// noopConfirm is the always-OK ConfirmFunc run_unsupported_test.go
// passes to Run. The stub returns ErrUnsupported before any menu
// wiring, so the answer doesn't matter -- what matters is that the
// extra parameter exists and the call site compiles.
func noopConfirm(_ context.Context, _, _ string) bool { return true }
