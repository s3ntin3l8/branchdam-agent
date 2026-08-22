// Package config loads branchdam-agent's own configuration: which branchDAM
// server to talk to, the shared agent API key, this workstation's
// self-asserted agentId (see the plan's contract-gap 6 -- there is no
// per-agent identity on the server side, just a shared secret), and the
// workstation-path -> container-path map preflight prints so an operator can
// eyeball it before the first real ingest.
package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// Config is branchdam-agent's on-disk configuration
// (~/.config/branchdam-agent/config.yaml per the plan, or wherever -config
// points).
type Config struct {
	Server       ServerConfig  `yaml:"server"`
	AgentID      string        `yaml:"agentId"`
	PathMappings []PathMapping `yaml:"pathMappings"`
	Ingest       IngestConfig  `yaml:"ingest"`
}

// IngestConfig configures M1's SD-card ingest core: where the two copies
// land and what relative-path template both derive from, and card-detection
// polling. ArchiveRoot/LocalEditRoot are workstation-native paths (this
// process writes to them with plain os.* calls, never through branchDAM's
// storage.Guard -- the agent runs on the workstation, not the server).
// ArchiveRoot's workstation-to-container translation for the
// EVENT_NODE_CREATED payload goes through PathMappings above; LocalEditRoot
// is never translated or sent to the server at all.
type IngestConfig struct {
	// ArchiveRoot is the workstation path backing the Tier-3 archive
	// destination (e.g. a mounted NAS share). Must have a matching
	// PathMapping so the agent can translate a written file's path into the
	// server-container, absolute, symlink-free form NodeCreatedPayload.FilePath
	// requires.
	ArchiveRoot string `yaml:"archiveRoot"`
	// LocalEditRoot is the workstation path for the local edit copy (fast
	// local/NVMe scratch). Never sent to the server -- the local copy is not
	// a server-tracked node.
	LocalEditRoot string `yaml:"localEditRoot"`
	// PathTemplate is the relative-path template both ArchiveRoot and
	// LocalEditRoot derive a destination path from, so the local copy
	// mirrors the archive subtree by construction. Supports
	// {yyyy}/{mm}/{dd}/{camera_model}/{original_name} placeholders -- see
	// internal/ingest's naming.go. Defaults to
	// "{yyyy}/{yyyy}-{mm}-{dd}_{camera_model}/{original_name}" when empty.
	PathTemplate string `yaml:"pathTemplate"`
	// CardRoots are the parent directories polled for newly mounted
	// removable volumes (e.g. "/media/$USER", "/run/media/$USER" on Linux,
	// "/Volumes" on macOS). Only used by the `ingest --watch` poll loop, not
	// by a direct `ingest --card <path>` invocation.
	CardRoots []string `yaml:"cardRoots"`
	// PollIntervalSecs is the card-detection poll interval; defaults to 2
	// (matching the plan's "poll every ~2s is sufficient") when <= 0.
	PollIntervalSecs int `yaml:"pollIntervalSecs"`
}

// ServerConfig is the branchDAM server this agent reports to.
type ServerConfig struct {
	BaseURL string `yaml:"baseUrl"`
	// APIKey is the shared secret sent as the raw X-API-Key header (no
	// scheme). The server rejects one under 32 chars with a 503 plain-text
	// body ("agent authentication is not configured") -- see
	// internal/branchdam's conformance contract doc.
	APIKey string `yaml:"apiKey"`
}

// PathMapping is one workstation-path -> container-path rewrite rule.
// branchDAM's own agent event payloads must carry server-container, absolute,
// symlink-free paths -- there is no server-side rewrite pass on agent
// payloads (unlike cfg.PathRewrites, whose only consumer is the project-file
// resolver) -- so the agent must translate a workstation path itself before
// it ever appears in a request body. preflight prints the resolved map so an
// operator can confirm it before the first real ingest.
type PathMapping struct {
	WorkstationPath string `yaml:"workstationPath"`
	ContainerPath   string `yaml:"containerPath"`
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{
			BaseURL: "http://localhost:8080",
		},
	}
}

// Load reads path, expands ${VAR} environment references, and parses it as
// YAML into Config, applying defaultConfig()'s zero-value defaults first.
func Load(path string) (Config, error) {
	cfg := defaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}

	expanded := expandEnv(string(data))

	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}

	return cfg, nil
}

// expandEnv replaces every ${VAR} with the environment variable's value,
// leaving the placeholder untouched if VAR is unset. It does NOT support a
// ":-default" separator -- everything between "${" and "}" is treated as one
// literal environment-variable name, so "${VAR:-default}" becomes the
// literal string "${VAR:-default}" whenever VAR is unset, not "default".
// Carried over from the go-http-template this repo was scaffolded from,
// where getting this wrong was a real bug once -- see the template's
// CLAUDE.md invariant of the same name.
func expandEnv(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(match string) string {
		key := strings.TrimSpace(envVarRe.FindStringSubmatch(match)[1])
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return match
	})
}
