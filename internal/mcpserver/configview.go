package mcpserver

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

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
// applied) and then puts on-disk placeholders back when the whole value
// is an unexpanded ${VAR}. A literal API key is replaced, and a database
// URL password is stripped, so those secrets never leave the process.
// Load expands ${VAR} when the variable is set, so the raw file is what
// decides which of the two cases this is.
func redactedConfig(path string) (map[string]any, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	raw, err := rawSecrets(path)
	if err != nil {
		return nil, err
	}
	switch {
	case wholePlaceholder.MatchString(raw.apiKey):
		cfg.Server.APIKey = raw.apiKey
	case raw.apiKey != "" || cfg.Server.APIKey != "":
		cfg.Server.APIKey = redactedKey
	}
	cfg.Integrations.ResolveDB.DatabaseURL = redactDatabaseURL(raw.databaseURL, cfg.Integrations.ResolveDB.DatabaseURL)
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

type rawSecretFields struct {
	apiKey      string
	databaseURL string
}

func rawSecrets(path string) (rawSecretFields, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return rawSecretFields{}, fmt.Errorf("read config: %w", err)
	}
	var raw struct {
		Server struct {
			APIKey string `yaml:"apiKey"`
		} `yaml:"server"`
		Integrations struct {
			ResolveDB struct {
				DatabaseURL string `yaml:"databaseUrl"`
			} `yaml:"resolvedb"`
		} `yaml:"integrations"`
	}
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return rawSecretFields{}, fmt.Errorf("parse config: %w", err)
	}
	return rawSecretFields{
		apiKey:      raw.Server.APIKey,
		databaseURL: raw.Integrations.ResolveDB.DatabaseURL,
	}, nil
}

// redactDatabaseURL keeps a whole ${VAR} placeholder, and otherwise
// replaces a URL password the way net/url.URL.Redacted does. A file DSN
// with no userinfo is unchanged.
func redactDatabaseURL(raw, loaded string) string {
	if wholePlaceholder.MatchString(strings.TrimSpace(raw)) {
		return strings.TrimSpace(raw)
	}
	u, err := url.Parse(loaded)
	if err != nil || u.User == nil {
		return loaded
	}
	if _, ok := u.User.Password(); !ok {
		return loaded
	}
	return u.Redacted()
}
