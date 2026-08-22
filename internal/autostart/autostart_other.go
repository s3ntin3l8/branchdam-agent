//go:build !windows && !darwin

package autostart

// Enable always returns ErrUnsupported on this platform.
func Enable(_ string, _ []string) error {
	return ErrUnsupported
}

// Disable always returns ErrUnsupported on this platform.
func Disable() error {
	return ErrUnsupported
}
