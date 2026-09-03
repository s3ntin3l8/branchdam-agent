//go:build !darwin && !windows && !linux

package netgate

func isMetered() (bool, error) {
	return false, nil
}
