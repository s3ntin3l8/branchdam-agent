//go:build linux

package netgate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/godbus/dbus/v5"
)

const (
	nmMeteredUnknown  = 0
	nmMeteredYes      = 1
	nmMeteredNo       = 2
	nmMeteredGuessYes = 3
	nmMeteredGuessNo  = 4
)

func isMetered() (bool, error) {
	metered, err := checkNetworkManagerDBus()
	if err == nil {
		return metered, nil
	}
	return checkSysfsWWAN()
}

func checkNetworkManagerDBus() (bool, error) {
	conn, err := dbus.SystemBus()
	if err != nil {
		return false, err
	}

	obj := conn.Object("org.freedesktop.NetworkManager", "/org/freedesktop/NetworkManager")

	// Check the primary connection first
	var primaryConnPath dbus.ObjectPath
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.NetworkManager", "PrimaryConnection").Store(&primaryConnPath); err == nil && primaryConnPath != "/" && primaryConnPath != "" {
		activeObj := conn.Object("org.freedesktop.NetworkManager", primaryConnPath)
		var meteredVal uint32
		if err := activeObj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.NetworkManager.Connection.Active", "Metered").Store(&meteredVal); err == nil {
			if meteredVal == nmMeteredYes || meteredVal == nmMeteredGuessYes {
				return true, nil
			}
			if meteredVal == nmMeteredNo || meteredVal == nmMeteredGuessNo {
				return false, nil
			}
		}
	}

	// Check global Metered property on NetworkManager
	var globalMetered uint32
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.NetworkManager", "Metered").Store(&globalMetered); err == nil {
		if globalMetered == nmMeteredYes || globalMetered == nmMeteredGuessYes {
			return true, nil
		}
		if globalMetered == nmMeteredNo || globalMetered == nmMeteredGuessNo {
			return false, nil
		}
	}

	// Check any active connections
	var activeConnections []dbus.ObjectPath
	if err := obj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.NetworkManager", "ActiveConnections").Store(&activeConnections); err == nil {
		allDefinitivelyUnmetered := len(activeConnections) > 0
		for _, acPath := range activeConnections {
			if acPath == "/" || acPath == "" {
				continue
			}
			acObj := conn.Object("org.freedesktop.NetworkManager", acPath)
			var meteredVal uint32
			if err := acObj.Call("org.freedesktop.DBus.Properties.Get", 0, "org.freedesktop.NetworkManager.Connection.Active", "Metered").Store(&meteredVal); err == nil {
				if meteredVal == nmMeteredYes || meteredVal == nmMeteredGuessYes {
					return true, nil
				}
				if meteredVal != nmMeteredNo && meteredVal != nmMeteredGuessNo {
					allDefinitivelyUnmetered = false
				}
			} else {
				allDefinitivelyUnmetered = false
			}
		}
		if allDefinitivelyUnmetered {
			return false, nil
		}
	}

	return false, fmt.Errorf("no active connection found via NetworkManager D-Bus")
}

var sysfsNetDir = "/sys/class/net"

func checkSysfsWWAN() (bool, error) {
	entries, err := os.ReadDir(sysfsNetDir)
	if err != nil {
		return false, nil
	}

	for _, entry := range entries {
		name := entry.Name()
		if name == "lo" {
			continue
		}

		ifaceDir := filepath.Join(sysfsNetDir, name)

		// Check if interface is up
		operstate, err := os.ReadFile(filepath.Join(ifaceDir, "operstate"))
		if err == nil && strings.TrimSpace(string(operstate)) != "up" {
			continue
		}

		// Known wwan/cellular interface prefixes
		if strings.HasPrefix(name, "wwan") || strings.HasPrefix(name, "wwp") || strings.HasPrefix(name, "cdc-wdm") || strings.HasPrefix(name, "ppp") {
			return true, nil
		}

		// Check device directory
		deviceDir := filepath.Join(ifaceDir, "device")
		if fi, err := os.Stat(deviceDir); err == nil && fi.IsDir() {
			driverPath, err := os.Readlink(filepath.Join(deviceDir, "driver"))
			if err == nil {
				driverName := filepath.Base(driverPath)
				if isWWANDriver(driverName) {
					return true, nil
				}
			}

			uevent, err := os.ReadFile(filepath.Join(deviceDir, "uevent"))
			if err == nil {
				ueventStr := string(uevent)
				if strings.Contains(ueventStr, "DEVTYPE=wwan") || strings.Contains(ueventStr, "DRIVER=qmi_wwan") || strings.Contains(ueventStr, "DRIVER=cdc_mbim") {
					return true, nil
				}
			}
		}

		// Check interface type (ARPHRD_PPP = 512)
		typeBytes, err := os.ReadFile(filepath.Join(ifaceDir, "type"))
		if err == nil && strings.TrimSpace(string(typeBytes)) == "512" {
			return true, nil
		}
	}

	return false, nil
}

func isWWANDriver(name string) bool {
	switch name {
	case "qmi_wwan", "cdc_mbim", "cdc_ncm", "cdc_ether", "sierra_net", "option", "huawei_cdc_ncm", "rndis_host", "ipheth":
		return true
	default:
		return false
	}
}
