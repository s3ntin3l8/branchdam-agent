//go:build !windows && !darwin

package tray

import (
	"context"
	"errors"
	"testing"
)

func TestRunUnsupportedReturnsError(t *testing.T) {
	r := NewRunner(&fakeIngester{}, nil, "")
	_, err := Run(context.Background(), r, nil, "http://127.0.0.1:38080/", fakeSelfUpdater{})
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("got %v, want ErrUnsupported", err)
	}
}
