// Package selfupdate wraps github.com/creativeprojects/go-selfupdate v1.6.0
// behind a small surface, gated entirely by config.SelfUpdateConfig.Enabled
// (default false -- see issue #3 and the plan doc's UI-stack section: "wire
// but off by default").
//
// This package used to be split behind a `-tags selfupdate` build tag,
// because go-selfupdate v1.6.0's top-level package (validate.go,
// unconditionally, regardless of which Validator you actually use) imports
// golang.org/x/crypto/openpgp, which govulncheck flags as GO-2026-5932
// ("unmaintained, unsafe by design ... Fixed in: N/A"), and that check runs
// inside this repo's REQUIRED `test-go / lint-and-test` check. That's no
// longer necessary: s3ntin3l8/.github/ci-go.yml's govulncheck-ignore input
// (added for this exact case, see s3ntin3l8/.github#49) lets this repo's CI
// suppress GO-2026-5932 specifically -- see issue #14 for the full history
// and re-check periodically whether go-selfupdate itself has dropped the
// unconditional openpgp import, at which point the ignore entry can go too.
package selfupdate

import (
	"context"
	"fmt"

	su "github.com/creativeprojects/go-selfupdate"
)

// CheckResult and its String() method live in result.go, untagged -- see
// that file's doc comment.

// Check asks repoSlug's ("owner/repo") GitHub releases for the latest
// release and compares it against currentVersion. It never downloads or
// applies anything -- see Apply for that -- so it is safe to call on every
// tray startup regardless of whether an operator has approved auto-apply.
// Callers must still gate the call itself behind
// config.SelfUpdateConfig.Enabled; Check does not re-check that flag.
func Check(ctx context.Context, repoSlug string, currentVersion string) (CheckResult, error) {
	repo := su.ParseSlug(repoSlug)

	release, found, err := su.DetectLatest(ctx, repo)
	if err != nil {
		return CheckResult{}, fmt.Errorf("selfupdate: detect latest release for %s: %w", repoSlug, err)
	}
	if !found {
		return CheckResult{CurrentVersion: currentVersion}, nil
	}

	result := CheckResult{CurrentVersion: currentVersion, LatestVersion: release.Version()}
	result.UpdateFound = release.GreaterThan(currentVersion)
	return result, nil
}

// Apply downloads and applies repoSlug's latest release in place of the
// binary at cmdPath, replacing the running process's own executable, and
// returns the version string that was applied. Never called unless an
// operator has both enabled self-update AND (a later UI affordance)
// confirmed the specific update -- Check/Apply are split for exactly that
// reason, matching go-selfupdate's own UpdateCommand/DetectLatest split.
func Apply(ctx context.Context, repoSlug string, cmdPath string, currentVersion string) (string, error) {
	repo := su.ParseSlug(repoSlug)
	release, err := su.UpdateCommand(ctx, cmdPath, currentVersion, repo)
	if err != nil {
		return "", fmt.Errorf("selfupdate: update %s from %s: %w", cmdPath, repoSlug, err)
	}
	return release.Version(), nil
}
