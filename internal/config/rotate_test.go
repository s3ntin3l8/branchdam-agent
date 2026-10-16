package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const rotateFixture = `# branchdam-agent config
server:
  # the branchDAM server
  baseUrl: "https://dam.example.com"
  apiKey: %s # paired device key
agentId: workstation-01
ingest:
  archiveRoot: ${ARCHIVE_ROOT}
`

func writeRotateFixture(t *testing.T, apiKeyValue string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(rotateFixture, "%s", apiKeyValue, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSetRotatedAPIKeyReplacesLiteralKey(t *testing.T) {
	path := writeRotateFixture(t, `"0123456789abcdef0123456789abcdef"`)
	newKey := "bdk_rotated_fedcba9876543210fedcba9876543210" // pragma: allowlist secret

	if err := SetRotatedAPIKey(path, newKey); err != nil {
		t.Fatalf("SetRotatedAPIKey: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{"# branchdam-agent config", "# the branchDAM server", "# paired device key", "${ARCHIVE_ROOT}", newKey} {
		if !strings.Contains(got, want) {
			t.Errorf("patched config lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "0123456789abcdef0123456789abcdef") { // pragma: allowlist secret
		t.Errorf("old key still present:\n%s", got)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %#o, want 0600", fi.Mode().Perm())
		}
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load after rotation: %v", err)
	}
	if cfg.Server.APIKey != newKey { // pragma: allowlist secret
		t.Error("Load does not return the rotated key")
	}
}

func TestSetRotatedAPIKeyLeavesPlaceholder(t *testing.T) {
	cases := map[string]string{"bare": `${BRANCHDAM_API_KEY}`, "quoted": `"${BRANCHDAM_API_KEY}"`, "partial": `"prefix-${KEY_SUFFIX}"`}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			path := writeRotateFixture(t, value)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			err = SetRotatedAPIKey(path, "bdk_rotated_fedcba9876543210fedcba9876543210") // pragma: allowlist secret
			if !errors.Is(err, ErrAPIKeyPlaceholder) {
				t.Fatalf("SetRotatedAPIKey = %v, want ErrAPIKeyPlaceholder", err)
			}
			if !strings.Contains(err.Error(), "environment variable") {
				t.Errorf("error should tell the operator to update the environment variable: %v", err)
			}
			if strings.Contains(err.Error(), "BRANCHDAM_API_KEY") || strings.Contains(err.Error(), "KEY_SUFFIX") {
				t.Errorf("error echoes the placeholder text, unlike checkSecretPlaceholder: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Errorf("config changed despite the placeholder:\n%s", after)
			}
		})
	}
}

func TestSetRotatedAPIKeyCreatesMissingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("agentId: a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetRotatedAPIKey(path, "bdk_rotated_fedcba9876543210fedcba9876543210"); err != nil { // pragma: allowlist secret
		t.Fatalf("SetRotatedAPIKey: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.APIKey != "bdk_rotated_fedcba9876543210fedcba9876543210" { // pragma: allowlist secret
		t.Error("server.apiKey not created")
	}
	if err := SetRotatedAPIKey(path, ""); err == nil {
		t.Error("an empty key must be refused")
	}
}
