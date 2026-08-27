package main

import (
	_ "embed"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// starterConfigYAML is written by both `init` and the tray's first-run
// setup wizard (tray.go). Deliberately not config.example.yaml -- that
// file's ${VAR} placeholders would immediately trip config.Validate's
// unexpanded-placeholder check, which exists to catch a real
// misconfiguration, not a freshly bootstrapped one.
//
//go:embed assets/config.starter.yaml
var starterConfigYAML []byte

// runInitCmd implements `branchdam-agent init`: the headless half of
// issue #30's "works out of the box" -- writes a starter config.yaml so an
// operator (or a script, or an SSH session with no display for the tray's
// zenity wizard) has something to edit instead of hand-authoring one from
// scratch or copying config.example.yaml and fighting its ${VAR}
// placeholders. Resolves the same way every other subcommand's -config
// flag does (config.ResolvePath), so running `init` with no flags in an
// empty directory writes to the per-user config directory, matching where
// a later flagless `tray`/`preflight` run will look for it.
func runInitCmd(args []string) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to write config file to (default: ./config.yaml if present, else the per-user config directory)")
	force := fs.Bool("force", false, "overwrite an existing config file")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	path, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent init: resolve config path: %v\n", err)
		return 1
	}

	if !*force {
		if _, statErr := os.Stat(path); statErr == nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent init: %s already exists -- use -force to overwrite\n", path)
			return 1
		}
	}

	if err := writeStarterConfig(path); err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent init: %v\n", err)
		return 1
	}

	fmt.Printf("branchdam-agent init: wrote a starter config to %s\n\n", path)
	fmt.Println("Next steps:")
	fmt.Println("  1. Edit server.baseUrl, server.apiKey (>= 32 chars), and agentId.")
	fmt.Println("  2. Set ingest.archiveRoot and ingest.localEditRoot to real paths, and add a")
	fmt.Println("     pathMappings entry covering ingest.archiveRoot.")
	fmt.Printf("  3. Run: branchdam-agent preflight -config %s\n", path)
	return 0
}

// writeStarterConfig creates path's parent directory (config.Load and
// config.DefaultPath deliberately never do this themselves -- see
// DefaultPath's doc comment) and writes starterConfigYAML to it.
func writeStarterConfig(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config directory %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, starterConfigYAML, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
