package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// ErrAPIKeyPlaceholder is returned by SetRotatedAPIKey when server.apiKey is
// an environment-variable placeholder: the key lives in the environment,
// not in config.yaml, so the agent cannot store a rotated one there.
var ErrAPIKeyPlaceholder = errors.New("server.apiKey is an environment-variable placeholder")

// SetRotatedAPIKey stores apiKey as server.apiKey in the config file at
// path -- the agent's half of branchDAM's rotated-key delivery
// (internal/branchdam/keyrotation.go). It goes through the same surgical
// yaml.Node patch as Patch (comments and other placeholders survive, the
// file is written atomically at 0600 and fsynced), under the same lock.
//
// When the file's server.apiKey value contains a ${VAR} placeholder it is
// left as is and the returned error wraps ErrAPIKeyPlaceholder with what
// the operator has to do: overwriting the placeholder with a literal key
// would silently move the secret out of the operator's environment
// management. The placeholder text itself is never put into the error,
// matching Validate's checkSecretPlaceholder.
func SetRotatedAPIKey(path, apiKey string) error {
	if apiKey == "" {
		return errors.New("config: refusing to store an empty server.apiKey")
	}
	patchMu.Lock()
	defer patchMu.Unlock()

	placeholder, err := apiKeyIsPlaceholder(path)
	if err != nil {
		return err
	}
	if placeholder {
		return fmt.Errorf("%w in %s, so the agent left it untouched: set that environment variable to this device's new API key (shown in branchDAM under Companion Pairing -> the device's credentials) and restart the agent before the current key expires", ErrAPIKeyPlaceholder, path)
	}
	return patchLocked(path, map[string]any{"server.apiKey": apiKey})
}

// apiKeyIsPlaceholder reports whether the raw (unexpanded) server.apiKey
// scalar in the file at path contains a ${VAR} placeholder. A missing file
// or key is not a placeholder.
func apiKeyIsPlaceholder(path string) (bool, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("config: read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return false, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return false, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return false, nil
	}
	root := doc.Content[0]
	i := findKey(root, "server")
	if i < 0 || root.Content[i+1].Kind != yaml.MappingNode {
		return false, nil
	}
	server := root.Content[i+1]
	j := findKey(server, "apiKey")
	if j < 0 {
		return false, nil
	}
	return envVarRe.MatchString(server.Content[j+1].Value), nil
}
