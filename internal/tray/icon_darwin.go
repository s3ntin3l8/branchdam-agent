//go:build darwin

package tray

import "image/color"

// buildTrayIconDarwin renders the monogram in white for macOS template-icon
// mode. When the tray title is empty, macOS treats the icon as a template
// image and automatically tints it to match the menu bar (dark in light
// mode, light in dark mode). The monogram's single-color shape is ideal
// for this — gray vs white is indistinguishable in template mode.
func buildTrayIconDarwin() []byte {
	return buildIconColor(false, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
}

// buildUnconfiguredTrayIconDarwin renders the monogram in white for the
// unconfigured state on macOS. In template mode macOS tints white to match
// the menu bar, making it indistinguishable from the normal-state white
// template — this is intentional: "not configured" is already communicated
// by the tray menu text, and a gray icon would be invisible on a dark menu
// bar.
func buildUnconfiguredTrayIconDarwin() []byte {
	return buildIconColor(false, color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
}
