//go:build linux

package netgate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckSysfsWWAN(t *testing.T) {
	tmpDir := t.TempDir()
	origSysfs := sysfsNetDir
	sysfsNetDir = tmpDir
	defer func() { sysfsNetDir = origSysfs }()

	// Case 1: Empty or standard eth0 (not metered)
	ethDir := filepath.Join(tmpDir, "eth0")
	if err := os.MkdirAll(ethDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ethDir, "operstate"), []byte("up\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	metered, err := checkSysfsWWAN()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metered {
		t.Errorf("expected eth0 to not be metered, got true")
	}

	// Case 2: Interface name starting with wwan0
	wwanDir := filepath.Join(tmpDir, "wwan0")
	if err := os.MkdirAll(wwanDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wwanDir, "operstate"), []byte("up\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	metered, err = checkSysfsWWAN()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !metered {
		t.Errorf("expected wwan0 to be metered, got false")
	}

	// Case 3: wwan0 is down -> not metered
	if err := os.WriteFile(filepath.Join(wwanDir, "operstate"), []byte("down\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	metered, err = checkSysfsWWAN()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if metered {
		t.Errorf("expected down wwan0 to not be metered, got true")
	}

	// Case 4: usb0 with device/uevent DEVTYPE=wwan
	usbDir := filepath.Join(tmpDir, "usb0")
	devDir := filepath.Join(usbDir, "device")
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usbDir, "operstate"), []byte("up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(devDir, "uevent"), []byte("DEVTYPE=wwan\nDRIVER=qmi_wwan\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	metered, err = checkSysfsWWAN()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !metered {
		t.Errorf("expected usb0 with DEVTYPE=wwan to be metered, got false")
	}
}
