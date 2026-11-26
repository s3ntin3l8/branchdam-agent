package mcpserver

import (
	"fmt"
	"os"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// redactedKey is what agent_config substitutes for a literal API key.
const redactedKey = "[redacted]"

// wholePlaceholder is an entire ${VAR} value, not a key that merely
// contains one. Those stay visible so the operator can see the file
// still points at the environment.
var wholePlaceholder = regexp.MustCompile(`^\$\{[^}]+\}$`)

// redactedConfig loads path the same way the agent does (defaults
// applied) and then puts the on-disk apiKey back when it is an
// unexpanded placeholder. A literal key is replaced so the secret never
// leaves the process. Load expands ${VAR} when the variable is set, so
// the raw file is what decides which of the two cases this is.
func redactedConfig(path string) (map[string]any, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	raw, err := rawAPIKey(path)
	if err != nil {
		return nil, err
	}
	switch {
	case wholePlaceholder.MatchString(raw):
		cfg.Server.APIKey = raw
	case raw != "" || cfg.Server.APIKey != "":
		cfg.Server.APIKey = redactedKey
	}
	body, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return out, nil
}

func rawAPIKey(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read config: %w", err)
	}
	var raw struct {
		Server struct {
			APIKey string `yaml:"apiKey"`
		} `yaml:"server"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parse config: %w", err)
	}
	return raw.Server.APIKey, nil
}
