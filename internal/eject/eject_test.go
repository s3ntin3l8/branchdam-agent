package eject

import (
	"testing"
)

func TestEjectErrorOnEmptyPath(t *testing.T) {
	// Calling Eject on empty/invalid path should fail gracefully on all supported platforms
	err := Eject("")
	if err == nil {
		t.Error("Eject(\"\") expected error, got nil")
	}
}
