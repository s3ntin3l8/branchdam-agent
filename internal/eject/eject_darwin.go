//go:build darwin

package eject

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

const ejectTimeout = 15 * time.Second

// Eject unmounts the volume at mountPath on macOS using diskutil unmountDisk.
func Eject(mountPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ejectTimeout)
	defer cancel()

	cleanPath := filepath.Clean(mountPath)
	cmd := exec.CommandContext(ctx, "diskutil", "unmountDisk", cleanPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("diskutil unmountDisk %s: %w (output: %s)", cleanPath, err, string(out))
	}
	return nil
}
