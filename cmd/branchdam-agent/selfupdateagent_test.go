package main

import (
	"context"
	"testing"
)

// TestSelfUpdateAgentRollbackAvailableFalseByDefault and
// TestSelfUpdateAgentRollbackFailsWithoutPrevious both rely on the real
// go test binary naturally having no ".previous"/".previous.version"
// sidecar next to it -- RollbackAvailable/Rollback resolve via
// os.Executable(), which can't be faked in-process, so these only
// exercise the "no rollback available" path. The full restore-from-
// backup logic is covered by internal/selfupdate's own rollback_test.go
// against fabricated InstallLayouts.

func TestSelfUpdateAgentRollbackAvailableFalseByDefault(t *testing.T) {
	a := &selfUpdateAgent{}
	if version, ok := a.RollbackAvailable(); ok {
		t.Errorf("expected RollbackAvailable=false, got version=%q ok=true", version)
	}
}

func TestSelfUpdateAgentRollbackFailsWithoutPrevious(t *testing.T) {
	a := &selfUpdateAgent{}
	if _, err := a.Rollback(context.Background()); err == nil {
		t.Error("expected Rollback to return an error when no previous version is available")
	}
}
