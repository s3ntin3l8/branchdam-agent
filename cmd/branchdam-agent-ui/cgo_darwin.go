//go:build darwin && production

package main

// This file exists solely to supply a linker flag wails v2.16.0's own
// darwin frontend needs but doesn't declare -- discovered when
// ci-cd.yml's build-darwin-full job first compiled this package under
// -tags production (v1.8.1's fix for the "Wails applications will not
// build without the correct build tags" bug), which is the first time
// any CI job linked the real (non-stub) darwin code at all.
//
// github.com/wailsapp/wails/v2/internal/frontend/desktop/darwin's
// WailsContext.h conditionally imports <UniformTypeIdentifiers/UTType.h>
// whenever the build SDK has it ("#if __has_include(...)", true on any
// SDK since macOS 11 / Xcode 12 -- CI's macos-26 runner ships Xcode 26.6),
// but none of that package's own #cgo LDFLAGS declarations
// (menuitem.go/callbacks.go/dialog.go/window.go/screen.go/menu.go/
// frontend.go/single_instance.go/notifications.go/inspector_dev.go) link
// -framework UniformTypeIdentifiers, the framework UTType actually lives
// in. The build failed at the final link step with "Undefined symbols
// for architecture arm64: _OBJC_CLASS_$_UTType" -- a genuine upstream gap
// in wails v2.16.0 (the latest available release as of this fix; there
// is no newer patch to bump to), not anything specific to this repo's
// own code.
//
// cgo LDFLAGS are cumulative across every package that goes into one
// final link, so this package declares the missing framework itself
// rather than patching the vendored module. No C symbol is referenced
// here; the only job of the cgo preamble immediately below is carrying
// the #cgo LDFLAGS line into the link -- it is a SEPARATE comment block
// from this one (a blank line breaks cgo's preamble association), since
// cgo feeds everything in the comment directly touching `import "C"` to
// the C compiler as literal source, not as documentation.
//
// Re-check whether this is still necessary the next time
// github.com/wailsapp/wails/v2 is upgraded past v2.16.0, and consider
// reporting it upstream if it hasn't been already.

// #cgo LDFLAGS: -framework UniformTypeIdentifiers
import "C"
