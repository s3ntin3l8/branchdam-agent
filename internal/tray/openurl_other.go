//go:build windows

package tray

// openURLs is never written on Windows: the OS protocol handler passes the
// URL in argv instead (cmd/branchdam-agent effectiveLaunchArgs). A nil channel
// blocks forever in Run's select, which is the intent.
var openURLs chan string

func registerOpenURLs() {}
