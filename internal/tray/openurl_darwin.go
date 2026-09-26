//go:build darwin

package tray

/*
#cgo LDFLAGS: -framework Cocoa
void branchdamRegisterOpenURLHandler(void);
*/
import "C"

import "log/slog"

// openURLs carries branchdam:// URLs from the Apple Event handler to Run. It is
// buffered so a URL that arrives before the tray finishes starting (a cold
// launch by the link) is held rather than dropped.
var openURLs = make(chan string, 4)

//export branchdamHandleOpenURL
func branchdamHandleOpenURL(cURL *C.char) {
	// Runs on the main thread inside the Apple Event callback: never block.
	// The single consumer can sit in a confirmation dialog, so when the
	// buffer is full drop the OLDEST queued link, not the newest: a page that
	// fires several links must not displace the operator's own click. The URL
	// embeds the API key, so only the fact of the drop is logged.
	u := C.GoString(cURL)
	for {
		select {
		case openURLs <- u:
			return
		default:
		}
		select {
		case <-openURLs:
			slog.Warn("branchdam:// link queue full; dropped the oldest queued link")
		default:
		}
	}
}

// registerOpenURLs installs the GetURL handler. It must run before
// [NSApp run] (systray.Run), or a cold-launch event is lost.
func registerOpenURLs() { C.branchdamRegisterOpenURLHandler() }
