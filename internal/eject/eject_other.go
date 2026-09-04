//go:build !darwin && !linux && !windows

package eject

// Eject returns ErrUnsupported on platforms other than darwin, linux, or windows.
func Eject(mountPath string) error {
	return ErrUnsupported
}
