//go:build darwin

package tray

/*
#cgo LDFLAGS: -framework Cocoa
void branchdamRegisterOpenURLHandler(void);
*/
import "C"

// openURLs carries branchdam:// URLs from the Apple Event handler to Run. It is
// buffered so a URL that arrives before the tray finishes starting (a cold
// launch by the link) is held rather than dropped.
var openURLs = make(chan string, 4)

//export branchdamHandleOpenURL
func branchdamHandleOpenURL(cURL *C.char) {
	// Runs on the main thread inside the Apple Event callback: never block.
	select {
	case openURLs <- C.GoString(cURL):
	default:
	}
}

// registerOpenURLs installs the GetURL handler. It must run before
// [NSApp run] (systray.Run), or a cold-launch event is lost.
func registerOpenURLs() { C.branchdamRegisterOpenURLHandler() }
