//go:build darwin

package netgate

import (
	"os/exec"
	"strings"
)

// scutilCmd runs scutil with the given args, overridable in tests.
var scutilCmd = func(args ...string) ([]byte, error) {
	return exec.Command("scutil", args...).Output()
}

// scutilDynamicStoreQuery queries scutil for a DynamicStore key, overridable in tests.
// Uses stdin piping instead of CGO-backed CoreFoundation bindings — avoids the CGO
// dependency but is slower per call and the text output format could change across
// macOS versions.
var scutilDynamicStoreQuery = func(key string) ([]byte, error) {
	cmd := exec.Command("scutil")
	cmd.Stdin = strings.NewReader("show " + key + "\n")
	return cmd.Output()
}

// networksetupCmd runs networksetup with the given args, overridable in tests.
var networksetupCmd = func(args ...string) ([]byte, error) {
	return exec.Command("networksetup", args...).Output()
}

func isMetered() (bool, error) {
	// 1. Check SCNetworkReachability flags via `scutil -r www.apple.com`
	out, err := scutilCmd("-r", "www.apple.com")
	if err == nil {
		s := string(out)
		// Flags: kSCNetworkReachabilityFlagsIsWWAN (0x40000), Transient Connection (0x4)
		if strings.Contains(s, "Is WWAN") || strings.Contains(s, "0x40000") || strings.Contains(s, "Transient Connection") {
			return true, nil
		}
	}

	// 2. Query primary interface via scutil DynamicStore (State:/Network/Global/IPv4)
	primaryIface := getPrimaryInterface()
	if primaryIface != "" {
		if isMeteredInterfaceName(primaryIface) {
			return true, nil
		}
		if isMeteredHardwarePort(primaryIface) {
			return true, nil
		}
	}

	// 3. Heuristic: check service names from `scutil --nc list` for PPP/cellular
	//    keywords. This matches on the human-assigned VPN service name, not actual
	//    connection state, so it may false-positive on oddly named services.
	ncOut, err := scutilCmd("--nc", "list")
	if err == nil {
		for _, line := range strings.Split(string(ncOut), "\n") {
			if strings.Contains(line, "(Connected)") {
				lower := strings.ToLower(line)
				if strings.Contains(lower, "ppp") || strings.Contains(lower, "cellular") || strings.Contains(lower, "wwan") || strings.Contains(lower, "hotspot") {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

func getPrimaryInterface() string {
	out, err := scutilDynamicStoreQuery("State:/Network/Global/IPv4")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PrimaryInterface :") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}

func isMeteredInterfaceName(iface string) bool {
	iface = strings.ToLower(iface)
	return strings.HasPrefix(iface, "pdp_ip") ||
		strings.HasPrefix(iface, "ppp") ||
		strings.HasPrefix(iface, "wwan") ||
		strings.HasPrefix(iface, "cellular")
}

func isMeteredHardwarePort(iface string) bool {
	out, err := networksetupCmd("-listallhardwareports")
	if err != nil {
		return false
	}
	blocks := strings.Split(string(out), "\n\n")
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		var port, device string
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Hardware Port:") {
				port = strings.TrimSpace(strings.TrimPrefix(line, "Hardware Port:"))
			} else if strings.HasPrefix(line, "Device:") {
				device = strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
			}
		}
		if device == iface && port != "" {
			portLower := strings.ToLower(port)
			if strings.Contains(portLower, "cellular") ||
				strings.Contains(portLower, "iphone") ||
				strings.Contains(portLower, "ipad") ||
				strings.Contains(portLower, "hotspot") ||
				strings.Contains(portLower, "wwan") ||
				strings.Contains(portLower, "ppp") {
				return true
			}
		}
	}
	return false
}
