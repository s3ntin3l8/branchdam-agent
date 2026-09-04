// Package eject handles OS-level unmounting and safe ejection of removable media
// (camera SD cards, CFexpress cards, USB drives) after verified ingest.
package eject

import (
	"errors"
)

// ErrUnsupported is returned when eject is called on an unsupported OS.
var ErrUnsupported = errors.New("eject: unsupported platform")
