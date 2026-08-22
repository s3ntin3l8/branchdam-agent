// Package selfupdate wraps github.com/creativeprojects/go-selfupdate v1.6.0
// behind a small surface, gated entirely by config.SelfUpdateConfig.Enabled
// (default false -- see issue #3 and the plan doc's UI-stack section: "wire
// but off by default"). Deliberately confined to this one file/package:
// go-selfupdate's GitHub source drags in go-github + oauth2, a meaningfully
// larger dependency-review surface than the rest of this repo, so keeping
// the import here means it stays cheaply excisable if that ever becomes a
// problem for the required dependency-review check without touching
// anything else that imports this package.
package selfupdate

import (
	"context"
	"fmt"

	su "github.com/creativeprojects/go-selfupdate"
)

// CheckResult is the outcome of a Check call -- what
// internal/tray.Status.SelfUpdateNote renders.
type CheckResult struct {
	CurrentVersion string
	LatestVersion  string
	UpdateFound    bool
}

func (r CheckResult) String() string {
	if !r.UpdateFound {
		return fmt.Sprintf("up to date (%s)", r.CurrentVersion)
	}
	return fmt.Sprintf("update available: %s -> %s", r.CurrentVersion, r.LatestVersion)
}

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
// binary at cmdPath, replacing the running process's own executable.
// Never called unless an operator has both enabled self-update AND
// (a later UI affordance) confirmed the specific update -- Check/Apply are
// split for exactly that reason, matching go-selfupdate's own
// UpdateCommand/DetectLatest split.
func Apply(ctx context.Context, repoSlug string, cmdPath string, currentVersion string) (*su.Release, error) {
	repo := su.ParseSlug(repoSlug)
	release, err := su.UpdateCommand(ctx, cmdPath, currentVersion, repo)
	if err != nil {
		return nil, fmt.Errorf("selfupdate: update %s from %s: %w", cmdPath, repoSlug, err)
	}
	return release, nil
}
