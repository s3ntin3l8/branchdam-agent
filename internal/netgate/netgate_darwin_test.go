//go:build darwin

package netgate

import (
	"testing"
)

func TestDarwinMetered_ReachabilityFlags(t *testing.T) {
	origScutil := scutilCmd
	defer func() { scutilCmd = origScutil }()

	scutilCmd = func(args ...string) ([]byte, error) {
		if len(args) == 2 && args[0] == "-r" {
			return []byte("Reachability Flags: Reachable, Transient Connection, Is WWAN (0x40003)\n"), nil
		}
		return []byte(""), nil
	}

	metered, err := isMetered()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !metered {
		t.Errorf("expected isMetered=true for WWAN reachability flags, got false")
	}
}

func TestDarwinMetered_HardwarePort(t *testing.T) {
	origScutil := scutilCmd
	origDynamic := scutilDynamicStoreQuery
	origSetup := networksetupCmd
	defer func() {
		scutilCmd = origScutil
		scutilDynamicStoreQuery = origDynamic
		networksetupCmd = origSetup
	}()

	scutilCmd = func(args ...string) ([]byte, error) {
		return []byte("Reachability Flags: Reachable (0x2)\n"), nil
	}
	scutilDynamicStoreQuery = func(key string) ([]byte, error) {
		return []byte("<dictionary> {\n  PrimaryInterface : en5\n}\n"), nil
	}
	networksetupCmd = func(args ...string) ([]byte, error) {
		return []byte("Hardware Port: iPhone USB\nDevice: en5\nEthernet Address: 00:00:00:00:00:00\n\nHardware Port: Wi-Fi\nDevice: en0\nEthernet Address: 00:00:00:00:00:01\n"), nil
	}

	metered, err := isMetered()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !metered {
		t.Errorf("expected isMetered=true for iPhone USB primary interface, got false")
	}
}
