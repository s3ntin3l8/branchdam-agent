//go:build selfupdate

// Package selfupdate wraps github.com/creativeprojects/go-selfupdate v1.6.0
// behind a small surface, gated entirely by config.SelfUpdateConfig.Enabled
// (default false -- see issue #3 and the plan doc's UI-stack section: "wire
// but off by default").
//
// The real implementation in THIS file only compiles into the binary with
// an explicit `-tags selfupdate` build flag -- an outer gate on top of the
// runtime config flag, not a replacement for it. This is a deliberate
// deviation from the issue's literal "off by default via config," forced by
// a real, unfixable finding: go-selfupdate v1.6.0's top-level package
// (validate.go, unconditionally, regardless of which Validator you actually
// use) imports golang.org/x/crypto/openpgp, which govulncheck flags as
// GO-2026-5932 ("unmaintained, unsafe by design ... Fixed in: N/A" -- there
// is no patched x/crypto version to upgrade to, and v1.6.0 is
// go-selfupdate's newest tag, so there is no upgrade path either).
// govulncheck runs unconditionally inside this repo's REQUIRED
// `test-go / lint-and-test` check (the shared
// s3ntin3l8/.github/ci-go.yml workflow, which exposes no vulncheck-ignore
// input this repo can reach) -- with the import compiled in by default, the
// required check fails on every push, permanently. Verified before adding
// the build tag: `govulncheck ./...` (no tags) is clean; `GOFLAGS=-tags=selfupdate
// govulncheck ./...` reproduces GO-2026-5932, confirming the tag is what's
// actually load-bearing here, not just a file that happens not to be
// imported by anything.
//
// selfupdate_stub.go is the `//go:build !selfupdate` counterpart everything
// else (default `make build`, `make check`, CI) actually compiles --
// same Check/Apply/CheckResult surface, so cmd/branchdam-agent/tray.go needs
// no build tag of its own and calls the same two functions either way.
//
// The proper fix is one of: go-selfupdate drops openpgp from its default
// import graph upstream, or ci-go.yml grows a vulncheck-ignore input (a
// shared-workflow change, not a per-repo workaround -- same pattern as this
// repo's other shared-workflow-only fixes). Track the compromise, don't
// just carry it silently: see issue #14.
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
