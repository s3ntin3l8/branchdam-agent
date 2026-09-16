// Package appbundle renders and assembles a macOS .app bundle around the
// branchdam-agent binary. Pure file/string operations, no macOS APIs, so
// it is unit-testable on any host including Linux CI -- the same
// discipline internal/autostart's plist rendering follows.
//
// RenderInfoPlist is called from two places that must never drift apart:
// tools/mkbundle at build time, and internal/selfupdate.Apply at update
// time (go-selfupdate only ever replaces the bundle's inner binary, never
// the bundle itself, so Apply rewrites Info.plist locally afterward to
// keep CFBundleVersion in sync -- see internal/selfupdate's doc comment).
// A single renderer is what guarantees both call sites produce identical
// output.
package appbundle

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/s3ntin3l8/branchdam-agent/internal/appicon"
	"github.com/s3ntin3l8/branchdam-agent/internal/autostart"
)

// BinaryName is the bundle's executable name, both on disk inside
// Contents/MacOS and as CFBundleExecutable.
const BinaryName = "branchdam-agent"

// UIBinaryName is cmd/branchdam-agent-ui's binary name inside
// Contents/MacOS, alongside BinaryName -- a second file in the bundle, not
// a second CFBundleExecutable (the UI binary is launched by full path from
// the tray's "Open branchDAM" menu item, never by Finder double-click, so
// the bundle has exactly one Info.plist-declared entry point either way).
// internal/selfupdate references this same constant (it already imports
// this package for RenderInfoPlist) so the two packages can never disagree
// on the name go-selfupdate looks for inside the release tarball.
const UIBinaryName = "branchdam-agent-ui"

// WinUIBinaryName is UIBinaryName's Windows counterpart -- cmd/branchdam-
// agent-ui's built binary name with the platform's own .exe extension.
// Exported from this package (rather than living only in
// internal/selfupdate, which already has an unexported winUIExe of its
// own) so internal/tray's "Open branchDAM" menu item (run_supported.go's
// launchUIBinary) can resolve the same name without importing
// internal/selfupdate -- SelfUpdater's own doc comment (internal/tray/
// tray.go) already established that import is unwanted (it pulls in
// golang.org/x/crypto/openpgp transitively). A Hermes review finding on
// PR #218: internal/tray had re-spelled this as a third independent
// literal, which could silently drift from internal/selfupdate's own copy
// if the binary were ever renamed -- the exact class of bug UIBinaryName's
// own doc comment above already guards against on the macOS side.
const WinUIBinaryName = "branchdam-agent-ui.exe"

// DisplayName is CFBundleName/CFBundleDisplayName -- what Finder and (were
// LSUIElement not set) the Dock would show.
const DisplayName = "branchDAM Agent"

// IconFileName is Contents/Resources/<IconFileName> -- CFBundleIconFile is
// written without the extension (Apple's own convention), so RenderInfoPlist
// strips it.
const IconFileName = "icon.icns"

var versionComponent = regexp.MustCompile(`^\d+$`)

// BundleVersion normalizes a release tag to the grammar CFBundleVersion /
// CFBundleShortVersionString require: 1-3 dot-separated non-negative
// integers, no leading "v", no prerelease/build suffix. "v1.0.1" ->
// "1.0.1"; "v1.0.1-rc1" -> "1.0.1"; anything that yields no numeric
// component at all (e.g. "dev", "manual-test") -> "0.0.0", since Apple
// rejects or silently misbehaves on a non-numeric value and a locally
// built binary is never what ships as a release bundle.
func BundleVersion(tag string) string {
	v := strings.TrimPrefix(tag, "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) > 3 {
		parts = parts[:3]
	}
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if versionComponent.MatchString(p) {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return "0.0.0"
	}
	return strings.Join(kept, ".")
}

// RenderInfoPlist renders the bundle's Info.plist. LSUIElement=1 is what
// keeps a tray-only app out of the Dock and the Cmd-Tab switcher (see
// docs/platform-support.md for the open question of whether this is
// sufficient -- unverified without a macOS host). CFBundleIdentifier
// reuses internal/autostart.Label so the bundle ID and the LaunchAgent
// Label are always the same string, not two names for the same thing that
// could drift. CFBundleIconFile names Contents/Resources/icon.icns (written
// once, at build time, by Write below) without its extension, matching
// Apple's own convention -- self-update's Apply/Rollback (see this file's
// package doc) call RenderInfoPlist again on an update, but neither ever
// touches Resources, so the icon persists unmodified across updates.
func RenderInfoPlist(version string) string {
	bv := BundleVersion(version)
	iconName := strings.TrimSuffix(IconFileName, filepath.Ext(IconFileName))
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleExecutable</key>
    <string>%s</string>
    <key>CFBundleIdentifier</key>
    <string>%s</string>
    <key>CFBundleName</key>
    <string>%s</string>
    <key>CFBundleDisplayName</key>
    <string>%s</string>
    <key>CFBundleIconFile</key>
    <string>%s</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleShortVersionString</key>
    <string>%s</string>
    <key>CFBundleVersion</key>
    <string>%s</string>
    <key>LSUIElement</key>
    <true/>
    <key>NSHighResolutionCapable</key>
    <true/>
    <key>LSMinimumSystemVersion</key>
    <string>11.0</string>
</dict>
</plist>
`, BinaryName, autostart.Label, BinaryName, DisplayName, iconName, bv, bv)
}

// Write assembles appDir (typically "branchdam-agent.app") as a fresh
// bundle around binPath: Contents/Info.plist, Contents/Resources/icon.icns,
// and a copy of binPath at Contents/MacOS/<BinaryName> with the executable
// bit preserved. appDir must not already exist -- Write never overwrites an
// existing bundle, so a build script that runs it twice fails loudly
// instead of merging state from a stale previous run.
//
// uiBinPath is optional (pass "" to skip it) -- when set, cmd/branchdam-
// agent-ui's binary is copied in too, at Contents/MacOS/<UIBinaryName>,
// a second file in the same directory rather than a second bundle
// executable (see UIBinaryName's own doc comment).
func Write(appDir, binPath, uiBinPath, version string) error {
	if _, err := os.Stat(appDir); err == nil {
		return fmt.Errorf("appbundle: %s already exists", appDir)
	}

	macOSDir := filepath.Join(appDir, "Contents", "MacOS")
	if err := os.MkdirAll(macOSDir, 0o755); err != nil {
		return fmt.Errorf("appbundle: create %s: %w", macOSDir, err)
	}
	resourcesDir := filepath.Join(appDir, "Contents", "Resources")
	if err := os.MkdirAll(resourcesDir, 0o755); err != nil {
		return fmt.Errorf("appbundle: create %s: %w", resourcesDir, err)
	}

	plistPath := filepath.Join(appDir, "Contents", "Info.plist")
	if err := os.WriteFile(plistPath, []byte(RenderInfoPlist(version)), 0o644); err != nil {
		return fmt.Errorf("appbundle: write %s: %w", plistPath, err)
	}

	icns, err := appicon.ICNS()
	if err != nil {
		return fmt.Errorf("appbundle: render icon: %w", err)
	}
	iconPath := filepath.Join(resourcesDir, IconFileName)
	if err := os.WriteFile(iconPath, icns, 0o644); err != nil {
		return fmt.Errorf("appbundle: write %s: %w", iconPath, err)
	}

	if err := copyExecutable(binPath, filepath.Join(macOSDir, BinaryName)); err != nil {
		return fmt.Errorf("appbundle: copy binary: %w", err)
	}

	if uiBinPath != "" {
		if err := copyExecutable(uiBinPath, filepath.Join(macOSDir, UIBinaryName)); err != nil {
			return fmt.Errorf("appbundle: copy UI binary: %w", err)
		}
	}

	return nil
}

func copyExecutable(src, dst string) (copyErr error) {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }() // read-only handle; a close error here can't lose data

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	// out is writable, so its Close error is load-bearing: data can still
	// be buffered and not yet flushed to disk at Close time. Only surface
	// it when nothing earlier already failed, matching io.Copy's error
	// taking priority.
	defer func() {
		if closeErr := out.Close(); copyErr == nil && closeErr != nil {
			copyErr = fmt.Errorf("close %s: %w", dst, closeErr)
		}
	}()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dst, err)
	}
	return nil
}
