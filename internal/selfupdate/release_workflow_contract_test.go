package selfupdate

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

type releaseWorkflowStep struct {
	Name string         `yaml:"name"`
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

type releaseWorkflowJob struct {
	Needs any                   `yaml:"needs"`
	Steps []releaseWorkflowStep `yaml:"steps"`
}

func workflowStep(t *testing.T, job releaseWorkflowJob, name string) (releaseWorkflowStep, int) {
	t.Helper()
	for i, step := range job.Steps {
		if step.Name == name {
			return step, i
		}
	}
	t.Fatalf("release workflow job is missing step %q", name)
	return releaseWorkflowStep{}, -1
}

func workflowUses(t *testing.T, job releaseWorkflowJob, action string) releaseWorkflowStep {
	t.Helper()
	for _, step := range job.Steps {
		if strings.HasPrefix(step.Uses, action+"@") {
			return step
		}
	}
	t.Fatalf("release workflow job does not use %q", action)
	return releaseWorkflowStep{}
}

// assertVersionedAsset pins BOTH halves of the release-asset naming
// contract at once, wherever runText builds or references an asset named
// "branchdam-agent-<version>-<archTail>": that the basename interpolates
// RELEASE_VERSION (a release can never ship version-less assets again --
// v1.8.0 did, see the fix that added this), AND that it still ends in the
// exact <os>-<arch><ext> tail go-selfupdate's own getSuffixes matches
// archives on (a closed set built as <os><sep><arch><ext>,
// github.com/creativeprojects/go-selfupdate@v1.6.0's detect.go). The tail
// is the load-bearing half: a rename that drops it makes every already-
// installed client stop finding updates, silently -- HasSuffix just
// returns false, DetectLatest just reports no release. Matching the
// EXPRESSION "${{ env.RELEASE_VERSION }}" rather than a concrete value
// also keeps this immune to the workflow_dispatch "manual-test" case,
// which carries no "v" prefix a version-shaped regex might assume.
func assertVersionedAsset(t *testing.T, runText, archTail string) {
	t.Helper()
	pattern := regexp.MustCompile(`branchdam-agent-\$\{\{\s*env\.RELEASE_VERSION\s*\}\}-` + regexp.QuoteMeta(archTail))
	if !pattern.MatchString(runText) {
		t.Errorf("release workflow step does not build a versioned asset ending %q (want to match %s)\n---\n%s", archTail, pattern, runText)
	}
}

func TestReleaseWorkflowMatchesUpdaterAttestationContract(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release-binaries.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]releaseWorkflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	attest, ok := workflow.Jobs["attest"]
	if !ok {
		t.Fatal("release workflow is missing attest job")
	}
	install, _ := workflowStep(t, attest, "Install cosign")
	if got := fmt.Sprint(install.With["cosign-release"]); got != "v2.6.1" {
		t.Errorf("cosign-release = %q, want v2.6.1", got)
	}
	sign, signIndex := workflowStep(t, attest, "Sign release assets (Sigstore keyless)")
	for _, want := range []string{
		`--output-signature "${asset}.sig"`,
		`--output-certificate "${asset}.cert"`,
		"cosign verify-blob",
		"--certificate-oidc-issuer 'https://token.actions.githubusercontent.com'",
		`--certificate-identity-regexp '^https://github\.com/s3ntin3l8/branchdam-agent/\.github/workflows/release-binaries\.yml@'`,
	} {
		if !strings.Contains(sign.Run, want) {
			t.Errorf("release signing step is missing updater contract %q", want)
		}
	}
	if signAt, verifyAt := strings.Index(sign.Run, "cosign sign-blob"), strings.Index(sign.Run, "cosign verify-blob"); signAt < 0 || verifyAt <= signAt {
		t.Errorf("release workflow must verify after signing (sign=%d verify=%d)", signAt, verifyAt)
	}
	_, storeIndex := workflowStep(t, attest, "Store complete release set")
	if storeIndex <= signIndex {
		t.Errorf("attest job must store release set after signing (sign=%d store=%d)", signIndex, storeIndex)
	}

	windows, ok := workflow.Jobs["build-windows"]
	if !ok {
		t.Fatal("release workflow is missing build-windows job")
	}
	packageStep, _ := workflowStep(t, windows, "Package")
	// The three archive MEMBER names are pinned as plain, unversioned
	// literals -- separate from (and orthogonal to) the archive's own
	// versioned name asserted below. self-update extracts by basename
	// (install.go's winKnownExes), so the members inside the zip must
	// never carry the version.
	if !strings.Contains(packageStep.Run, "branchdam-agent.exe branchdam-agent-tray.exe branchdam-agent-ui.exe") {
		t.Error("Windows package must contain the console, tray, and UI executables")
	}
	assertVersionedAsset(t, packageStep.Run, "windows-amd64.zip")
	windowsArtifact := workflowUses(t, windows, "actions/upload-artifact")
	if !strings.Contains(fmt.Sprint(windowsArtifact.With["path"]), "windows-amd64.zip") {
		t.Error("Windows build artifact must include the packaged updater ZIP")
	}

	linux, ok := workflow.Jobs["build-linux"]
	if !ok {
		t.Fatal("release workflow is missing build-linux job")
	}
	linuxPackage, _ := workflowStep(t, linux, "Package")
	assertVersionedAsset(t, linuxPackage.Run, "linux-amd64.tar.gz")

	darwinForAssets, ok := workflow.Jobs["build-darwin"]
	if !ok {
		t.Fatal("release workflow is missing build-darwin job")
	}
	darwinPackage, _ := workflowStep(t, darwinForAssets, "Package")
	assertVersionedAsset(t, darwinPackage.Run, "darwin-arm64.tar.gz")
	darwinDMG, _ := workflowStep(t, darwinForAssets, "Build DMG")
	assertVersionedAsset(t, darwinDMG.Run, "darwin-arm64.dmg")
	darwinArtifact := workflowUses(t, darwinForAssets, "actions/upload-artifact")
	darwinPath := fmt.Sprint(darwinArtifact.With["path"])
	// @actions/glob@0.6.1's * does not cross / -- the DMG is written to
	// dist/ by create-dmg, so its glob must be dist/-prefixed to match.
	if !strings.Contains(darwinPath, "branchdam-agent-*-darwin-arm64.tar.gz") ||
		!strings.Contains(darwinPath, "dist/branchdam-agent-*-darwin-arm64.dmg") {
		t.Error("Darwin artifact must include tarball (at root) and dist/-prefixed DMG")
	}

	// The attest job is not a build job and has no default RELEASE_VERSION
	// from env context inheritance -- it needs its own, or the "Assemble
	// release set and checksums" step's cp lines silently reference the
	// PRE-fix unversioned filenames the build jobs no longer produce,
	// failing the whole release at the last step.
	assemble, _ := workflowStep(t, attest, "Assemble release set and checksums")
	for _, tail := range []string{"linux-amd64.tar.gz", "windows-amd64.zip", "windows-amd64-setup.exe", "darwin-arm64.tar.gz", "darwin-arm64.dmg"} {
		if tail == "windows-amd64-setup.exe" {
			// Not an <os>-<arch><ext> suffix go-selfupdate would ever
			// match (it isn't an archive) -- assert its versioning
			// directly rather than through assertVersionedAsset's
			// suffix-matching contract.
			if !regexp.MustCompile(`branchdam-agent-\$\{\{\s*env\.RELEASE_VERSION\s*\}\}-setup\.exe`).MatchString(assemble.Run) {
				t.Error("attest job's cp lines do not reference a versioned setup.exe")
			}
			continue
		}
		assertVersionedAsset(t, assemble.Run, tail)
	}

	upload, ok := workflow.Jobs["upload"]
	if !ok {
		t.Fatal("release workflow is missing upload job")
	}
	if got := fmt.Sprint(upload.Needs); got != "attest" {
		t.Errorf("upload needs = %q, want attest", got)
	}
	refGuard, _ := workflowStep(t, upload, "Refuse mismatched manual release source")
	if !strings.Contains(refGuard.Run, `test "$GITHUB_REF" = "refs/tags/$RELEASE_TAG"`) {
		t.Error("manual release upload must reject a mismatched tag ref")
	}
	workflowStep(t, upload, "Upload complete release set")
}

// TestReleaseWorkflowSignsDarwinBundleBeforePackaging pins the ordering
// invariant a real bug briefly violated: ad-hoc signing must run BEFORE
// both darwin packaging steps (the .tar.gz self-update payload and the
// .dmg), or whichever one was packaged first silently ships unsigned. This
// exact regression shipped once -- caught by review, not by any existing
// test, since this file's other ordering assertions only ever covered the
// attest job -- see resign.go's own doc comment for the runtime-side half
// of this fix (resignAppBundle re-signs after self-update mutates the
// bundle, which only matters if the shipped bundle was actually signed to
// begin with).
func TestReleaseWorkflowSignsDarwinBundleBeforePackaging(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release-binaries.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]releaseWorkflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	darwin, ok := workflow.Jobs["build-darwin"]
	if !ok {
		t.Fatal("release workflow is missing build-darwin job")
	}

	sign, signIndex := workflowStep(t, darwin, "Ad-hoc sign bundle")
	for _, want := range []string{
		"codesign --force --sign -",
		"codesign --verify --strict",
	} {
		if !strings.Contains(sign.Run, want) {
			t.Errorf("darwin sign step is missing %q", want)
		}
	}

	_, packageIndex := workflowStep(t, darwin, "Package")
	if packageIndex <= signIndex {
		t.Errorf("build-darwin must sign before packaging the tarball (sign=%d package=%d) -- otherwise the self-update payload ships unsigned", signIndex, packageIndex)
	}

	_, dmgIndex := workflowStep(t, darwin, "Build DMG")
	if dmgIndex <= signIndex {
		t.Errorf("build-darwin must sign before building the .dmg (sign=%d dmg=%d) -- otherwise the manual-install download ships unsigned", signIndex, dmgIndex)
	}
}

// TestReleaseWorkflowChecksumAssetIsNeverVersioned pins the one asset name
// in the release set that must NEVER change: go-selfupdate matches every
// OTHER release asset by suffix (see assertVersionedAsset's own doc
// comment), tolerant of an inserted version segment, but it looks up the
// checksum/validation asset by EXACT name equality
// (findValidationAsset/ChecksumValidator.GetValidationAssetName in
// github.com/creativeprojects/go-selfupdate@v1.6.0's detect.go). A miss
// there returns ErrValidationAssetNotFound, and DetectLatest reports NO
// release found at all -- not a partial success, not a warning. The name
// this repo commits to is ChecksumAsset (selfupdate.go), baked into every
// already-shipped binary; it cannot be changed retroactively for a client
// already in the field. This test asserts the release workflow's own
// redirect target for "Assemble release set and checksums" is the
// LITERAL ChecksumAsset value, with no RELEASE_VERSION interpolation --
// the one release asset name in this whole file that must stay boring.
func TestReleaseWorkflowChecksumAssetIsNeverVersioned(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release-binaries.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]releaseWorkflowJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &workflow); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}

	attest, ok := workflow.Jobs["attest"]
	if !ok {
		t.Fatal("release workflow is missing attest job")
	}
	assemble, _ := workflowStep(t, attest, "Assemble release set and checksums")

	literalRedirect := "> " + ChecksumAsset
	if !strings.Contains(assemble.Run, literalRedirect) {
		t.Errorf("attest job does not redirect checksums to the literal %q (ChecksumAsset) -- got:\n%s", literalRedirect, assemble.Run)
	}
	if strings.Contains(assemble.Run, "RELEASE_VERSION") && strings.Contains(assemble.Run, "SHA256SUMS") {
		// Loose guard against a future edit accidentally interpolating
		// the checksum filename itself while touching the surrounding
		// versioned cp lines -- SHA256SUMS.txt must never appear next to
		// a RELEASE_VERSION substitution in the same step.
		for _, line := range strings.Split(assemble.Run, "\n") {
			if strings.Contains(line, "SHA256SUMS") && strings.Contains(line, "RELEASE_VERSION") {
				t.Errorf("a line referencing SHA256SUMS.txt must never also interpolate RELEASE_VERSION: %q", line)
			}
		}
	}
}

func TestWindowsInstallerDesktopLaunchContract(t *testing.T) {
	b, err := os.ReadFile("../../installer/windows/branchdam-agent.nsi")
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, want := range []string{
		`!define MUI_FINISHPAGE_RUN_PARAMETERS "tray"`,
		`CreateShortCut "$SMPROGRAMS\${PRODUCT_NAME}\${PRODUCT_NAME}.lnk" "$INSTDIR\branchdam-agent-tray.exe" "tray"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("installer is missing desktop launch contract %q", want)
		}
	}
	if strings.Contains(script, `Section "Start at login"`) {
		t.Error("installer must not manage autostart independently of tray.startOnLogin")
	}
}
