package selfupdate

import (
	"fmt"
	"os"
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
	if !strings.Contains(packageStep.Run, "zip -j branchdam-agent-windows-amd64.zip branchdam-agent.exe branchdam-agent-tray.exe") {
		t.Error("Windows package must contain both console and tray executables")
	}
	windowsArtifact := workflowUses(t, windows, "actions/upload-artifact")
	if !strings.Contains(fmt.Sprint(windowsArtifact.With["path"]), "dist/branchdam-agent-windows-amd64.zip") {
		t.Error("Windows build artifact must include the packaged updater ZIP")
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
