//go:build windows

package netgate

import (
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	modiphlpapi                    = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetNetworkConnectivityHint = modiphlpapi.NewProc("GetNetworkConnectivityHint")
)

type nlNetworkConnectivityHint struct {
	ConnectivityLevel    uint32
	ConnectivityCost     uint32
	ApproachingDataLimit byte
	OverDataLimit        byte
	Roaming              byte
	CostModifier         byte
}

const (
	networkConnectivityCostHintUnknown      = 0
	networkConnectivityCostHintUnrestricted = 1
	networkConnectivityCostHintFixed        = 2
	networkConnectivityCostHintVariable     = 3
)

func isMetered() (bool, error) {
	if err := procGetNetworkConnectivityHint.Find(); err != nil {
		return false, err
	}
	var hint nlNetworkConnectivityHint
	r1, _, _ := procGetNetworkConnectivityHint.Call(uintptr(unsafe.Pointer(&hint)))
	if r1 != 0 {
		return false, syscall.Errno(r1)
	}
	if hint.ConnectivityCost == networkConnectivityCostHintFixed ||
		hint.ConnectivityCost == networkConnectivityCostHintVariable ||
		hint.OverDataLimit != 0 ||
		hint.Roaming != 0 {
		return true, nil
	}
	return false, nil
}
