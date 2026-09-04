//go:build windows

package eject

import (
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// IOCTL_STORAGE_EJECT_MEDIA is the control code for ejecting media from a removable drive.
// CTL_CODE(IOCTL_STORAGE_BASE(0x2D), 0x0202, METHOD_BUFFERED(0), FILE_READ_ACCESS(1)) = 0x002D4808.
const IOCTL_STORAGE_EJECT_MEDIA = 0x002D4808

// Eject sends IOCTL_STORAGE_EJECT_MEDIA via DeviceIoControl to safely eject
// the volume at mountPath on Windows.
func Eject(mountPath string) error {
	volPath, err := volumeDevicePath(mountPath)
	if err != nil {
		return fmt.Errorf("eject %s: %w", mountPath, err)
	}

	pathPtr, err := windows.UTF16PtrFromString(volPath)
	if err != nil {
		return fmt.Errorf("eject %s: UTF16PtrFromString: %w", mountPath, err)
	}

	handle, err := windows.CreateFile(
		pathPtr,
		windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
		nil,
		windows.OPEN_EXISTING,
		0,
		0,
	)
	if err != nil {
		// Fallback to 0 (query access) if read/write is denied
		handle, err = windows.CreateFile(
			pathPtr,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			nil,
			windows.OPEN_EXISTING,
			0,
			0,
		)
		if err != nil {
			return fmt.Errorf("eject %s: open volume %s: %w", mountPath, volPath, err)
		}
	}
	defer windows.CloseHandle(handle)

	var bytesReturned uint32
	err = windows.DeviceIoControl(
		handle,
		IOCTL_STORAGE_EJECT_MEDIA,
		nil,
		0,
		nil,
		0,
		&bytesReturned,
		nil,
	)
	if err != nil {
		return fmt.Errorf("eject %s: DeviceIoControl IOCTL_STORAGE_EJECT_MEDIA: %w", mountPath, err)
	}
	return nil
}

// volumeDevicePath formats a Windows path into a volume device path like `\\.\D:`.
func volumeDevicePath(mountPath string) (string, error) {
	clean := filepath.Clean(mountPath)
	if strings.HasPrefix(clean, `\\.\`) {
		return clean, nil
	}
	vol := filepath.VolumeName(clean)
	if vol == "" {
		return "", fmt.Errorf("no drive letter or volume name found in %q", mountPath)
	}
	vol = strings.TrimSuffix(vol, `\`)
	vol = strings.TrimSuffix(vol, `/`)
	return `\\.\` + vol, nil
}
