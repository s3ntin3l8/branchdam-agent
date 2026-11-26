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

// redactDatabaseURL keeps a whole ${VAR} placeholder. Otherwise a userinfo
// password is replaced (net/url.URL.Redacted, or scheme://[redacted]@host
// when the DSN does not parse) and a password query parameter is replaced
// too: lib/pq accepts ?password= as a real credential. A DSN with no
// userinfo and no password parameter is returned unchanged.
func redactDatabaseURL(raw, loaded string) string {
	if wholePlaceholder.MatchString(strings.TrimSpace(raw)) {
		return strings.TrimSpace(raw)
	}
	return redactPasswordParam(redactUserinfo(loaded))
}

func redactUserinfo(loaded string) string {
	u, err := url.Parse(loaded)
	if err == nil {
		if u.User == nil {
			return loaded
		}
		if _, ok := u.User.Password(); !ok {
			return loaded
		}
		return u.Redacted()
	}
	return redactUserinfoText(loaded)
}

// redactUserinfoText replaces scheme://userinfo@rest with
// scheme://[redacted]@rest. The last @ splits userinfo from the host,
// matching net/url. A DSN with no userinfo is returned as-is: there is
// no password in that position to hide, and collapsing it would drop the
// path the operator is looking at.
func redactUserinfoText(dsn string) string {
	scheme, rest, ok := strings.Cut(dsn, "://")
	if !ok {
		return dsn
	}
	at := strings.LastIndex(rest, "@")
	if at < 0 {
		return dsn
	}
	return scheme + "://" + redactedKey + "@" + rest[at+1:]
}

// redactPasswordParam replaces a password= value (query string or keyword
// DSN) with xxxxx. The key match is case-insensitive and does not fire
// inside a longer identifier. The scan stays on the original string:
// strings.ToLower is not length-preserving for some non-ASCII runes, and
// indexes into a lowercased copy would miss the key or slice the wrong bytes.
func redactPasswordParam(dsn string) string {
	const key = "password="
	var b strings.Builder
	start := 0
	for start < len(dsn) {
		rel := indexFold(dsn[start:], key)
		if rel < 0 {
			b.WriteString(dsn[start:])
			break
		}
		idx := start + rel
		if idx > 0 && isKeyChar(dsn[idx-1]) {
			b.WriteString(dsn[start : idx+len(key)])
			start = idx + len(key)
			continue
		}
		b.WriteString(dsn[start:idx])
		b.WriteString("password=xxxxx")
		val := idx + len(key)
		for val < len(dsn) && dsn[val] != '&' && dsn[val] != ' ' {
			val++
		}
		start = val
	}
	return b.String()
}

// indexFold returns the byte index of sub in s, matching ASCII case
// only by comparing equal-length windows. sub is ASCII.
func indexFold(s, sub string) int {
	if sub == "" || len(s) < len(sub) {
		return -1
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(sub)], sub) {
			return i
		}
	}
	return -1
}

func isKeyChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_'
}
