package netgate

// isMeteredFn is an internal function pointer to allow test mocking.
var isMeteredFn = isMetered

// IsMetered reports whether the current default/primary network connection
// is metered (e.g. mobile hotspot, cellular/WWAN, or tethered interface).
func IsMetered() (bool, error) {
	return isMeteredFn()
}
