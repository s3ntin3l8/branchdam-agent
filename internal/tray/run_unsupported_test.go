//go:build !windows && !darwin

package tray

import (
	"context"
	"errors"
	"testing"
)

func TestRunUnsupportedReturnsError(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	_, err := Run(context.Background(), r, "http://127.0.0.1:38080/", fakeSelfUpdater{}, fakeSettings{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}
