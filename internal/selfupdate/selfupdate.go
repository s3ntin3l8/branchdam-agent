// Package selfupdate wraps github.com/creativeprojects/go-selfupdate
// v1.6.0 behind a small surface, gated entirely by config's
// SelfUpdateConfig.Enabled (default true -- a read-only GitHub API call,
// never a download or a write; see the tray's own gating in
// cmd/branchdam-agent/tray.go). Applying an update found by a check is a
// separate, always-explicit action this flag does not by itself
// authorize.
//
// Every download this package makes is verified against two integrity
// controls, in order:
//
//  1. A Sigstore keyless signature on the archive (and the
//     SHA256SUMS.txt), produced at publish time by
//     .github/workflows/release-binaries.yml via `cosign sign-blob` and
//     verified in-process here by sigstoreVerify (sigstore.go) using
//     github.com/sigstore/sigstore-go against the embedded Fulcio
//     trusted root. Proves the release was built by this repo's signed
//     release workflow, not by a non-maintainer who gained release-page
//     write access. Rekor / SCT / TSA verifiers are deliberately
//     disabled -- we want self-update to work for a host that is
//     online only briefly, and the load-bearing threat (release-page
//     forgery) is already closed by cert+issuer+SAN check alone.
//  2. A SHA-256 checksum on every file inside the archive, via
//     go-selfupdate's ChecksumValidator against the release's
//     SHA256SUMS.txt. This is the original integrity control
//     (pre-PR #131) and stays as defense in depth: even if a future
//     cert-chain regression slipped past the Sigstore check, an
//     archive with a forged .sig/.cert but a real SHA256 entry still
//     passes (and vice versa).
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
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/Masterminds/semver/v3"
	su "github.com/creativeprojects/go-selfupdate"
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
// Each target is applied through its OWN go-selfupdate Updater instance,
// configured with an OldSavePath of "<target>.previous" -- go-selfupdate's
// own su.Config.OldSavePath is a single fixed string per Updater, and this
// package's single shared u.up Updater is reused across every target in
// layout, so setting it there would make a Windows install's second
// UpdateTo call (the primary exe) silently overwrite the sibling exe's
// just-saved backup at the same path. A fresh per-target Updater (cheap:
// no network, just a struct) sidesteps that without needing a shared
// mutable field. This does mean the release archive is still downloaded
// and checksum-validated once per target (three times on Windows, two on
// macOS now that the UI binary ships as a sibling on both) -- accepted
// as a known cost rather than reimplementing go-selfupdate's unexported
// download/validate pipeline ourselves, which would move this repo's only
// integrity control (see this file's own doc comment) out of the
// library's hands and into a hand-rolled copy that silently stops
// tracking upstream if the configured Validator ever changes. See
// docs/platform-support.md's Known gaps.
//
// The multi-target swap is all-or-nothing (see applyAllTargets): the
// version being replaced (currentVersion) is recorded in the rollback
// sidecar next to layout.Primary's own backup only AFTER every target has
// swapped, and a failure on any target -- or on the sidecar write itself
// -- restores every target already swapped from its fresh ".previous"
// backup, so a partial failure never leaves the install at mixed versions
// or advertises a rollback whose label doesn't match the backups on disk.
// See rollback.go's PreviousVersion/HasRollback/Rollback for how that
// sidecar is used.
//
// ctx should not be tied to a signal-derived cancellation for the whole
// call: a cancel landing between the sibling apply and the primary apply
// would abort the update and unwind the already-swapped sibling (see
// applyAllTargets), wasting the download. Callers should use
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

	// NEW: preflight. Fetches .sig + .cert for every release target's
	// asset. On failure, the cache is left empty and the per-target
	// Validator below will refuse to apply.
	cache := newAttestCache()
	preflightAssets := make([]assetAttestation, 0, 1+len(layout.Siblings))
	// Each go-selfupdate target uses its OWN Updater (see
	// comment below) and all targets share the same archive, so
	// the (Name, URL) pair is the same for every entry -- the dedupe
	// in sigstorePreflight ensures only one network round trip even
	// for the Windows two-target case.
	for range layout.orderedTargets() {
		preflightAssets = append(preflightAssets, assetAttestation{
			Name: release.AssetName,
			URL:  release.AssetURL,
		})
	}
	if err := sigstorePreflight(ctx, preflightAssets, cache); err != nil {
		return "", err
	}

	err = applyAllTargets(layout, currentVersion, func(target string) error {
		targetUpdater, err := su.NewUpdater(su.Config{
			// The composed validator runs SHA256 (defense in depth)
			// then Sigstore verify. The cache is populated by the
			// preflight above.
			Validator:   &sigstoreValidator{cache: cache},
			OldSavePath: target + rollbackSuffix,
		})
		if err != nil {
			return fmt.Errorf("selfupdate: configure updater for %s: %w", target, err)
		}
		if err := targetUpdater.UpdateTo(ctx, release, target); err != nil {
			return fmt.Errorf("selfupdate: apply %s to %s: %w", release.Version(), target, err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	// Every target is already live on the new version by this point, so a
	// failure inside updateBundleInfoPlist's own (best-effort) resign step
	// can't undo that -- see its doc comment.
	if layout.InfoPlist != "" {
		if err := updateBundleInfoPlist(layout.InfoPlist, release.Version()); err != nil {
			return "", err
		}
	}

	return release.Version(), nil
}

// applyAllTargets swaps every target in layout (siblings first, Primary
// last) through applyOne, which must leave the replaced binary at
// "<target>.previous" on success (Apply's per-target OldSavePath), and
// makes the whole swap all-or-nothing for every failure the process
// survives. The rollback label (the version sidecar) is never left
// describing backups that don't match it: it either matches them or is
// absent.
//
//   - A prior Apply's label is withdrawn before any target is touched.
//     The first swap overwrites that target's ".previous", so the prior
//     rollback can't stay coherent once swapping starts; withdrawing it
//     first means a failure to delete it stops the update with nothing
//     replaced. If the first target then fails with its ".previous"
//     still in place (download, checksum, signature), nothing was
//     replaced and the prior label is written back. go-selfupdate
//     deletes that ".previous" just before moving the current binary
//     aside, so a failure from there on leaves the label withdrawn.
//   - If applyOne fails on any later target, every target already swapped
//     is restored from its own fresh ".previous" (most recent first) and
//     that backup is removed, so the install ends back on currentVersion
//     everywhere with no rollback advertised.
//   - Only once every target has swapped is the label written. A
//     label-write failure is treated like a target failure (every target
//     is restored): a new version without a matching rollback label would
//     otherwise orphan the fresh backups.
//
// A restore failure during that unwind is joined into the returned error
// and leaves that target's ".previous" in place for manual recovery.
//
// A crash (power loss, kill) part way through is not covered, though it
// still never leaves a wrong label: the prior label is already withdrawn,
// so no rollback is offered. A crash before the Primary swaps can leave
// the siblings on the new version; running update again finishes the job,
// since the Primary still reports currentVersion and the update is offered
// again. A crash after the Primary swaps (for a single-target install, any
// crash after its one swap) leaves every target on the new version with no
// rollback until the next update.
func applyAllTargets(layout InstallLayout, currentVersion string, applyOne func(target string) error) error {
	sidecar := layout.Primary + rollbackVersionSuffix
	prior, err := withdrawRollback(sidecar)
	if err != nil {
		return err
	}

	var swapped []string
	for _, target := range layout.orderedTargets() {
		if err := applyOne(target); err != nil {
			err = recoverFailedSwap(target, err)
			if len(swapped) == 0 {
				return reinstateRollback(target, sidecar, prior, err)
			}
			return restoreSwapped(swapped, err)
		}
		swapped = append(swapped, target)
	}

	if err := os.WriteFile(sidecar, []byte(currentVersion), 0o600); err != nil {
		return restoreSwapped(swapped, fmt.Errorf("selfupdate: record previous version for rollback: %w", err))
	}
	return nil
}

// withdrawRollback removes a prior Apply's rollback label and returns its
// content (nil when there was none, or it couldn't be read) so
// reinstateRollback can write it back.
func withdrawRollback(sidecar string) ([]byte, error) {
	prior, err := os.ReadFile(sidecar)
	if err != nil {
		prior = nil
	}
	if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("selfupdate: clear stale rollback metadata: %w", err)
	}
	return prior, nil
}

// reinstateRollback writes the prior rollback label back after the first
// target failed, if that target's ".previous" is still there: nothing was
// replaced, so the prior rollback still matches every backup. A failure to
// write it back is joined into cause; the label then stays withdrawn.
func reinstateRollback(target, sidecar string, prior []byte, cause error) error {
	if prior == nil {
		return cause
	}
	if _, err := os.Lstat(target + rollbackSuffix); err != nil {
		return cause
	}
	if err := os.WriteFile(sidecar, prior, 0o600); err != nil {
		return errors.Join(cause, fmt.Errorf("selfupdate: restore rollback metadata: %w", err))
	}
	return cause
}

// recoverFailedSwap puts target back when a failed swap left nothing at
// its path: go-selfupdate moves the current binary aside to ".previous"
// before moving the new one in, and when that second rename fails and its
// own rename back fails too, target is missing and the binary being
// replaced sits only at ".previous". (update.RollbackError reports that
// case, but only on Apply's unwrapped error, so the file system is checked
// instead.) Any failure to put it back is joined into cause.
func recoverFailedSwap(target string, cause error) error {
	if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
		return cause
	}
	if err := os.Rename(target+rollbackSuffix, target); err != nil {
		return errors.Join(cause, fmt.Errorf("selfupdate: put %s back after failed swap: %w", target, err))
	}
	return cause
}

// restoreSwapped undoes a partial applyAllTargets: every target in swapped
// is restored from its ".previous" backup in reverse swap order, and the
// consumed backup removed. cause is always returned (joined with any
// restore failure) so the caller still reports the original apply error.
func restoreSwapped(swapped []string, cause error) error {
	errs := []error{cause}
	for i := len(swapped) - 1; i >= 0; i-- {
		target := swapped[i]
		if err := restoreOne(target); err != nil {
			errs = append(errs, fmt.Errorf("selfupdate: restore %s after failed apply: %w", target, err))
			continue
		}
		if err := os.Remove(target + rollbackSuffix); err != nil && !os.IsNotExist(err) {
			slog.Warn("selfupdate: could not remove consumed backup after failed apply", "path", target+rollbackSuffix, "err", err)
		}
	}
	return errors.Join(errs...)
}
