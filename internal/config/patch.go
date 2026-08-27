package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Patch applies dotted-key updates (e.g. "tray.startOnLogin", "server.baseUrl")
// to the YAML file at path and writes it back. Unlike a yaml.Marshal(cfg)
// round-trip, it edits the raw, un-expanded document tree in place: every
// comment survives, and every ${VAR} placeholder this call does not
// explicitly overwrite stays a literal placeholder rather than being baked
// in as its expanded (and, for server.apiKey, secret) value -- see Load's
// expandEnv, which only ever runs on a copy read into memory, never on what
// Patch writes back to disk.
//
// Each change value must be a type yaml.Node.Encode accepts directly: bool,
// string, int, or []string are all a tray settings menu needs today. A key
// path that doesn't exist yet is created (as a block-style YAML mapping);
// a key path that exists has its comments preserved by transplanting them
// from the old value node onto the new one, since a mapping entry's
// association between a HeadComment and its *key* node (not the value
// node) already survives untouched -- Patch never touches key nodes for an
// existing entry, only swaps the value.
//
// The file is written atomically (temp file in the same directory, mode
// 0600, then rename) so a crash mid-write can never leave config.yaml
// truncated or half-written. Mode 0600 matters here specifically because
// setting server.apiKey from a dialog writes that secret to disk in
// plaintext -- callers offering that must say so.
//
// Known cosmetic limitation: blank lines between top-level sections are not
// part of yaml.v3's Node/comment model, so a Patch call collapses them.
// This is a formatting-only side effect, verified (patch_test.go) to never
// touch comment text, comment count, or any untouched value -- accepted
// rather than chased, since the alternative (a bespoke line-oriented
// splicer that still has to create nested keys correctly) is meaningfully
// riskier for a purely cosmetic gap.
func Patch(path string, changes map[string]any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("config: patch: read %s: %w", path, err)
	}

	var doc yaml.Node
	if len(strings.TrimSpace(string(raw))) == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}}
	} else if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("config: patch: parse %s: %w", path, err)
	}
	if len(doc.Content) == 0 {
		doc.Kind = yaml.DocumentNode
		doc.Content = []*yaml.Node{{Kind: yaml.MappingNode, Tag: "!!map"}}
	}

	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("config: patch: %s: document root is not a YAML mapping", path)
	}

	// Sorted purely for deterministic output across repeated calls with the
	// same change set (golden-test friendliness) -- correctness doesn't
	// depend on order, since each dotted key is looked up by name at every
	// level, not by position.
	keys := make([]string, 0, len(changes))
	for k := range changes {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, dotted := range keys {
		if err := setPath(root, strings.Split(dotted, "."), changes[dotted]); err != nil {
			return fmt.Errorf("config: patch: set %q: %w", dotted, err)
		}
	}

	out, err := marshalNode(&doc)
	if err != nil {
		return fmt.Errorf("config: patch: encode %s: %w", path, err)
	}

	if err := writeFileAtomic(path, out, 0o600); err != nil {
		return fmt.Errorf("config: patch: %w", err)
	}
	return nil
}

// setPath walks (creating as needed) the mapping nodes named by segments
// and sets the final segment's value, preserving that leaf's existing
// comments when it already exists.
func setPath(mapping *yaml.Node, segments []string, value any) error {
	key := segments[0]
	idx := findKey(mapping, key)

	if len(segments) == 1 {
		valNode, err := scalarNodeFor(value)
		if err != nil {
			return fmt.Errorf("encode value for %q: %w", key, err)
		}
		if idx >= 0 {
			old := mapping.Content[idx+1]
			valNode.HeadComment = old.HeadComment
			valNode.LineComment = old.LineComment
			valNode.FootComment = old.FootComment
			mapping.Content[idx+1] = valNode
		} else {
			mapping.Content = append(mapping.Content, keyNode(key), valNode)
		}
		return nil
	}

	var child *yaml.Node
	if idx >= 0 {
		child = mapping.Content[idx+1]
		if child.Kind != yaml.MappingNode {
			return fmt.Errorf("%q is a %s, not a mapping", key, nodeKindName(child.Kind))
		}
	} else {
		child = &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		mapping.Content = append(mapping.Content, keyNode(key), child)
	}
	return setPath(child, segments[1:], value)
}

func findKey(mapping *yaml.Node, key string) int {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return i
		}
	}
	return -1
}

func keyNode(key string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
}

func scalarNodeFor(value any) (*yaml.Node, error) {
	var n yaml.Node
	if err := n.Encode(value); err != nil {
		return nil, err
	}
	return &n, nil
}

func nodeKindName(k yaml.Kind) string {
	switch k {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}

// marshalNode re-encodes doc with a 2-space indent, matching
// config.example.yaml's existing style (yaml.Marshal's own default is 4).
func marshalNode(doc *yaml.Node) ([]byte, error) {
	var buf strings.Builder
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return []byte(buf.String()), nil
}

// writeFileAtomic writes data to a temp file in path's directory and
// renames it into place, so a crash mid-write never leaves path
// truncated. The temp file's Close error is surfaced (not discarded) since
// data can still be buffered and unflushed at Close time -- same
// discipline as internal/appbundle.copyExecutable and
// internal/selfupdate.checkWritable.
func writeFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".branchdam-agent-config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file: %w", werr)
	}
	if cerr := tmp.Chmod(mode); cerr != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file: %w", cerr)
	}
	// tmp is writable and holds the whole new file's data, unflushed until
	// Close -- unlike a read-only handle's Close, this one is load-bearing
	// and must not be silently discarded.
	if cerr := tmp.Close(); cerr != nil {
		return fmt.Errorf("close temp file: %w", cerr)
	}

	if rerr := os.Rename(tmpPath, path); rerr != nil {
		return fmt.Errorf("rename temp file into place: %w", rerr)
	}
	return nil
}
