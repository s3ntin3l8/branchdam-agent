//go:build !windows

package protocolhandler

// Register is a no-op off Windows: macOS registers the scheme through the app
// bundle's Info.plist (CFBundleURLTypes) and Linux has no installer.
func Register(string) error { return nil }
