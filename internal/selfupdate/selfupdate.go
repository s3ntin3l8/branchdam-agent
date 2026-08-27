// Package selfupdate wraps github.com/creativeprojects/go-selfupdate
// v1.6.0 behind a small surface, gated entirely by config's
// SelfUpdateConfig.Enabled (default false -- see the tray's own gating in
// cmd/branchdam-agent/tray.go).
//
// Every download this package makes is verified against the release's
// SHA256SUMS.txt via a su.ChecksumValidator, which is why Updater always
// constructs go-selfupdate's Updater itself (NewUpdater below) rather than
// ever calling the package-level su.DetectLatest/su.UpdateCommand helpers
// -- those use a validator-less default Updater with no integrity check
// at all. This is the *only* integrity control in this repo's release
// pipeline; nothing is code-signed or notarized (see internal/appbundle's
// doc comment for why the macOS bundle specifically must stay that way).
//
// Apply never calls go-selfupdate's own UpdateCommand helper, for two
// reasons UpdateCommand's own implementation doesn't handle: it compares
// versions with Equal, not GreaterThan, so it would happily downgrade a
// workstation if the "latest" release were older than the running build;
// and it only ever targets a single path, but a Windows install has two
// executables (see InstallLayout) that must never drift to different
// versions.
//
// This package used to be split behind a `-tags selfupdate` build tag,
// because go-selfupdate v1.6.0's top-level package (validate.go,
// unconditionally, regardless of which Validator you actually use)
// imports golang.org/x/crypto/openpgp, which govulncheck flags as
// GO-2026-5932 ("unmaintained, unsafe by design ... Fixed in: N/A"), and
// that check runs inside this repo's REQUIRED `test-go / lint-and-test`
// check. That's no longer necessary: s3ntin3l8/.github/ci-go.yml's
// govulncheck-ignore input (added for this exact case, see
// s3ntin3l8/.github#49) lets this repo's CI suppress GO-2026-5932
// specifically -- see issue #14 for the full history and re-check
// periodically whether go-selfupdate itself has dropped the unconditional
// openpgp import, at which point the ignore entry can go too.
package selfupdate

import (
	"context"
	"fmt"
	"os"

	"github.com/Masterminds/semver/v3"
	su "github.com/creativeprojects/go-selfupdate"

	"github.com/s3ntin3l8/branchdam-agent/internal/appbundle"
)

// ChecksumAsset is the release asset every published archive is verified
// against. Must stay byte-identical to the filename
// .github/workflows/release-binaries.yml's `upload` job emits.
const ChecksumAsset = "SHA256SUMS.txt"

// Updater is a configured go-selfupdate client: a ChecksumValidator
// pinned to ChecksumAsset, plus the repo slug releases are published
// from. The same instance backs both Check and Apply, so a validator
// misconfiguration can never let Check advertise an update Apply would
// then refuse to verify.
type Updater struct {
	repoSlug string
	up       *su.Updater
}

// NewUpdater configures a client against repoSlug ("owner/name"). Never
// returns a client without a checksum validator -- see this package's doc
// comment for why that's non-negotiable.
func NewUpdater(repoSlug string) (*Updater, error) {
	up, err := su.NewUpdater(su.Config{
		Validator: &su.ChecksumValidator{UniqueFilename: ChecksumAsset},
	})
	if err != nil {
		return nil, fmt.Errorf("selfupdate: configure updater: %w", err)
	}
	return &Updater{repoSlug: repoSlug, up: up}, nil
}

// Check asks the repo's GitHub releases for the latest release and
// compares it against currentVersion. It never downloads or applies
// anything, so it is safe to call on every tray startup (and on a
// periodic ticker) regardless of whether an operator has approved
// auto-apply. Returns ErrVersionNotSemver without making any network call
// if currentVersion isn't parseable semver.
func (u *Updater) Check(ctx context.Context, currentVersion string) (CheckResult, error) {
	if _, err := semver.NewVersion(currentVersion); err != nil {
		return CheckResult{CurrentVersion: currentVersion}, fmt.Errorf("%w: %q", ErrVersionNotSemver, currentVersion)
	}

	repo := su.ParseSlug(u.repoSlug)
	release, found, err := u.up.DetectLatest(ctx, repo)
	if err != nil {
		return CheckResult{}, fmt.Errorf("selfupdate: detect latest release for %s: %w", u.repoSlug, err)
	}
	if !found {
		return CheckResult{CurrentVersion: currentVersion}, nil
	}

	result := CheckResult{CurrentVersion: currentVersion, LatestVersion: release.Version()}
	result.UpdateFound = release.GreaterThan(currentVersion)
	return result, nil
}

// Apply downloads and applies the latest release to every path in layout,
// siblings first and layout.Primary last (see InstallLayout's doc
// comment), and returns the version string that was applied. Refuses
// with ErrVersionNotSemver, ErrNotNewer, or ErrTargetNotWritable before
// downloading anything. When layout.InfoPlist is set (the running binary
// is inside a macOS .app bundle), the plist is rewritten locally after
// the binary swap succeeds -- go-selfupdate only ever replaces the
// bundle's inner binary, never the bundle itself, so nothing else keeps
// CFBundleVersion in sync.
//
// ctx should not be tied to a signal-derived cancellation for the whole
// call: a cancel landing between the sibling apply and the primary apply
// would leave the two at different versions. Callers should use
// context.WithoutCancel plus their own timeout instead of passing a
// shutdown context straight through.
func (u *Updater) Apply(ctx context.Context, currentVersion string, layout InstallLayout) (string, error) {
	if _, err := semver.NewVersion(currentVersion); err != nil {
		return "", fmt.Errorf("%w: %q", ErrVersionNotSemver, currentVersion)
	}
	for _, dir := range layout.targetDirs() {
		if err := checkWritable(dir); err != nil {
			return "", err
		}
	}

	repo := su.ParseSlug(u.repoSlug)
	release, found, err := u.up.DetectLatest(ctx, repo)
	if err != nil {
		return "", fmt.Errorf("selfupdate: detect latest release for %s: %w", u.repoSlug, err)
	}
	if !found || !release.GreaterThan(currentVersion) {
		return "", ErrNotNewer
	}

	for _, target := range layout.orderedTargets() {
		if err := u.up.UpdateTo(ctx, release, target); err != nil {
			return "", fmt.Errorf("selfupdate: apply %s to %s: %w", release.Version(), target, err)
		}
	}

	if layout.InfoPlist != "" {
		plist := appbundle.RenderInfoPlist(release.Version())
		if err := os.WriteFile(layout.InfoPlist, []byte(plist), 0o644); err != nil {
			return "", fmt.Errorf("selfupdate: update %s: %w", layout.InfoPlist, err)
		}
	}

	return release.Version(), nil
}
