package netgate

import (
	"errors"
	"testing"
)

func TestIsMetered_Mock(t *testing.T) {
	orig := isMeteredFn
	defer func() { isMeteredFn = orig }()

	isMeteredFn = func() (bool, error) {
		return true, nil
	}

	metered, err := IsMetered()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !metered {
		t.Errorf("expected metered=true, got false")
	}

	isMeteredFn = func() (bool, error) {
		return false, errors.New("lookup failed")
	}

	metered, err = IsMetered()
	if err == nil {
		t.Errorf("expected error, got nil")
	}
	if metered {
		t.Errorf("expected metered=false on error, got true")
	}
}
