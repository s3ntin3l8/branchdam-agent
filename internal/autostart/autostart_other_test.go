//go:build !windows && !darwin

package autostart

import (
	"errors"
	"testing"
)

func TestEnableDisableUnsupportedOnThisPlatform(t *testing.T) {
	if err := Enable("/usr/local/bin/branchdam-agent", nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
	if err := Disable(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}
