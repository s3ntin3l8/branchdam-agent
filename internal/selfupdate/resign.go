package selfupdate

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// resignAppBundle re-signs a macOS .app bundle with an ad-hoc signature.
// Apply and Rollback both call it, immediately after rewriting
// Contents/Info.plist, because an ad-hoc `codesign` signature doesn't just
// gate notarization -- it seals a manifest of every file's hash under
// Contents/Resources and Info.plist itself
// (Contents/_CodeSignature/CodeResources). Neither Apply's inner-binary
// swap nor its Info.plist rewrite touches that seal, so without
// re-signing here, `codesign --verify` on the bundle fails immediately
// after every successful update or rollback (a Hermes review finding on
// the PR that introduced ad-hoc signing -- see docs/platform-support.md's
// Ad-hoc signing section for what an ad-hoc signature covers and doesn't).
//
// A package-level var, matching sigstore.go's fetchClient seam, so tests
// can substitute a fake and assert it was called with the right bundle
// path and that its error propagates -- without needing a real macOS host
// or a real codesign binary. defaultResignAppBundle no-ops on any GOOS
// other than darwin: codesign doesn't exist there, and layout.InfoPlist
// being set is only ever a signal of a real macOS bundle path in
// production, even though BundlePath's own pure-string check (see
// install.go) means a fabricated InstallLayout can set it on Linux CI too
// -- that's exactly what lets this whole call site stay exercised by
// Linux-run tests via the var seam, the same as everything else in this
// package.
var resignAppBundle = defaultResignAppBundle

func defaultResignAppBundle(bundleDir string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("codesign", "--force", "--sign", "-", bundleDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("selfupdate: re-sign %s: %w: %s", bundleDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}
