//go:build linux

package eject

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const ejectTimeout = 15 * time.Second

// Eject resolves the block device backing mountPath from /proc/mounts,
// unmounts it with udisksctl unmount -b <dev>, and then powers it off
// with udisksctl power-off -b <dev>.
func Eject(mountPath string) error {
	ctx, cancel := context.WithTimeout(context.Background(), ejectTimeout)
	defer cancel()

	mountsData, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return fmt.Errorf("read /proc/mounts: %w", err)
	}

	dev, err := resolveDeviceFromMounts(mountPath, mountsData)
	if err != nil {
		return err
	}

	unmountCmd := exec.CommandContext(ctx, "udisksctl", "unmount", "-b", dev)
	if out, err := unmountCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("udisksctl unmount -b %s: %w (output: %s)", dev, err, string(out))
	}

	powerOffCmd := exec.CommandContext(ctx, "udisksctl", "power-off", "-b", dev)
	if out, err := powerOffCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("udisksctl power-off -b %s: %w (output: %s)", dev, err, string(out))
	}

	return nil
}

// resolveDeviceFromMounts parses /proc/mounts content and returns the device
// backing mountPath. Handles octal escapes (e.g. \040 for space).
func resolveDeviceFromMounts(mountPath string, mountsData []byte) (string, error) {
	if mountPath == "" {
		return "", fmt.Errorf("mount path is empty")
	}
	target := filepath.Clean(mountPath)
	lines := bytes.Split(mountsData, []byte("\n"))

	var bestDev string
	var bestLen int

	for _, line := range lines {
		fields := bytes.Fields(line)
		if len(fields) < 2 {
			continue
		}
		dev := string(fields[0])
		mp := unescapeOctal(string(fields[1]))
		cleanMP := filepath.Clean(mp)

		// Never match root "/" as a removable volume mount point
		if cleanMP == "/" {
			continue
		}

		if cleanMP == target {
			return dev, nil
		}
		prefix := cleanMP + string(filepath.Separator)
		if strings.HasPrefix(target, prefix) && len(cleanMP) > bestLen {
			bestDev = dev
			bestLen = len(cleanMP)
		}
	}

	if bestDev != "" {
		return bestDev, nil
	}
	return "", fmt.Errorf("device not found in /proc/mounts for mount path %q", mountPath)
}

func unescapeOctal(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) && isOctalDigit(s[i+1]) && isOctalDigit(s[i+2]) && isOctalDigit(s[i+3]) {
			val, err := strconv.ParseUint(s[i+1:i+4], 8, 8)
			if err == nil {
				buf.WriteByte(byte(val))
				i += 3
				continue
			}
		}
		buf.WriteByte(s[i])
	}
	return buf.String()
}

func isOctalDigit(b byte) bool {
	return b >= '0' && b <= '7'
}
