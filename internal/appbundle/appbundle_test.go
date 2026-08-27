package appbundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam-agent/internal/autostart"
)

func TestBundleVersion(t *testing.T) {
	cases := map[string]string{
		"v1.0.1":       "1.0.1",
		"1.0.1":        "1.0.1",
		"v1.0.1-rc1":   "1.0.1",
		"v2.0.0+build": "2.0.0",
		"v1.2.3.4":     "1.2.3",
		"dev":          "0.0.0",
		"manual-test":  "0.0.0",
		"":             "0.0.0",
	}
	for in, want := range cases {
		if got := BundleVersion(in); got != want {
			t.Errorf("BundleVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderInfoPlist(t *testing.T) {
	plist := RenderInfoPlist("v1.2.3")

	for _, want := range []string{
		"<key>CFBundleExecutable</key>\n    <string>branchdam-agent</string>",
		"<key>CFBundleIdentifier</key>\n    <string>" + autostart.Label + "</string>",
		"<key>CFBundleShortVersionString</key>\n    <string>1.2.3</string>",
		"<key>CFBundleVersion</key>\n    <string>1.2.3</string>",
		"<key>LSUIElement</key>\n    <true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("RenderInfoPlist output missing %q\ngot:\n%s", want, plist)
		}
	}
}

func TestWrite(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "src-binary")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	appDir := filepath.Join(dir, "branchdam-agent.app")
	if err := Write(appDir, binPath, "v1.0.0"); err != nil {
		t.Fatal(err)
	}

	plistPath := filepath.Join(appDir, "Contents", "Info.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Errorf("Info.plist not written: %v", err)
	}

	innerBin := filepath.Join(appDir, "Contents", "MacOS", BinaryName)
	info, err := os.Stat(innerBin)
	if err != nil {
		t.Fatalf("inner binary not written: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("inner binary not executable: mode %v", info.Mode())
	}

	if err := Write(appDir, binPath, "v1.0.0"); err == nil {
		t.Error("Write over an existing bundle should fail, got nil error")
	}
}
