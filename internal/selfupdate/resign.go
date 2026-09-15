package selfupdate

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
)

// resignTimeout bounds the codesign subprocess -- matching sigstore.go's
// fetchClient timeout precedent for an external-process call this package
// makes -- so a wedged codesign can't hang Apply/Rollback indefinitely.
// Neither call site has a natural ctx to thread through here (Apply's is
// scoped to the whole multi-target download, Rollback has none at all), so
// a fixed timeout is the pragmatic choice.
const resignTimeout = 30 * time.Second

// resignAppBundle re-signs a macOS .app bundle with an ad-hoc signature.
// updateBundleInfoPlist calls it, immediately after rewriting
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
// path, and that a failure there is logged rather than returned (see
// updateBundleInfoPlist's own doc comment for why that's best-effort) --
// without needing a real macOS host or a real codesign binary.
// defaultResignAppBundle no-ops on any GOOS other than darwin: codesign
// doesn't exist there, and layout.InfoPlist being set is only ever a
// signal of a real macOS bundle path in production, even though
// BundlePath's own pure-string check (see install.go) means a fabricated
// InstallLayout can set it on Linux CI too -- that's exactly what lets
// this whole call site stay exercised by Linux-run tests via the var
// seam, the same as everything else in this package.
var resignAppBundle = defaultResignAppBundle

func defaultResignAppBundle(bundleDir string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), resignTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "codesign", "--force", "--sign", "-", bundleDir).CombinedOutput()
	if err != nil {
		return fmt.Errorf("selfupdate: re-sign %s: %w: %s", bundleDir, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// updateBundleInfoPlist rewrites infoPlist to render version, then
// re-signs the enclosing bundle -- the exact sequence Apply and Rollback
// both need after mutating a macOS bundle's contents (an inner-binary swap
// for Apply, a restore from backup for Rollback), extracted once so a
// change to that sequence -- or a test of it -- covers both callers
// instead of drifting between two copies.
//
// The resign step is deliberately best-effort: by the time this runs, the
// binary swap/restore has already fully succeeded, so a resign failure
// only leaves the bundle's signature seal exactly as stale as it was
// before this feature existed (still launchable, just not
// `codesign --verify` clean) -- treating it as fatal would report a
// genuinely successful update or rollback as failed, which is strictly
// worse. Logged via slog.Warn, not swallowed, so it's diagnosable.
func updateBundleInfoPlist(infoPlist, version string) error {
	plist := appbundle.RenderInfoPlist(version)
	if err := os.WriteFile(infoPlist, []byte(plist), 0o644); err != nil {
		return fmt.Errorf("selfupdate: update %s: %w", infoPlist, err)
	}
	if err := resignAppBundle(filepath.Dir(filepath.Dir(infoPlist))); err != nil {
		slog.Warn("selfupdate: could not re-sign app bundle", "path", infoPlist, "err", err)
	}
	return nil
}
