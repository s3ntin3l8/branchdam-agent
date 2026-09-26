//go:build windows

package protocolhandler

import (
	"fmt"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	protocolKeyPath = `Software\Classes\branchdam`
	commandKeyPath  = protocolKeyPath + `\shell\open\command`
	protocolTitle   = "URL:branchDAM pairing"
)

// Register writes HKCU\Software\Classes\branchdam pointing at execPath's
// handler. It is idempotent: when the registration already matches it writes
// nothing and sends no shell notification.
func Register(execPath string) error {
	cmd := Command(HandlerExe(execPath))
	if registered(cmd) {
		return nil
	}
	if err := setDefault(protocolKeyPath, protocolTitle); err != nil {
		return err
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, protocolKeyPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("protocolhandler: open %s: %w", protocolKeyPath, err)
	}
	err = k.SetStringValue("URL Protocol", "")
	_ = k.Close()
	if err != nil {
		return fmt.Errorf("protocolhandler: set URL Protocol: %w", err)
	}
	if err := setDefault(commandKeyPath, cmd); err != nil {
		return err
	}
	// MSDN: registering a protocol requires SHCNE_ASSOCCHANGED so Explorer
	// sees it without a reboot.
	shell := windows.NewLazySystemDLL("shell32.dll").NewProc("SHChangeNotify")
	_, _, _ = shell.Call(0x08000000, 0, 0, 0)
	return nil
}

func registered(cmd string) bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, commandKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer func() { _ = k.Close() }()
	got, _, err := k.GetStringValue("")
	return err == nil && got == cmd
}

func setDefault(path, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("protocolhandler: open %s: %w", path, err)
	}
	defer func() { _ = k.Close() }()
	if err := k.SetStringValue("", value); err != nil {
		return fmt.Errorf("protocolhandler: set %s: %w", path, err)
	}
	return nil
}
