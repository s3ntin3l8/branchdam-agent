package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/luminar"
	"github.com/s3ntin3l8/branchdam-agent/internal/nodeindex"
)

// runLuminarSyncCmd implements `branchdam-agent luminar-sync`. See issue #34
// and docs/luminar-catalog.md: this reads a Luminar Neo catalog read-only
// with a row-extraction query VERIFIED against a real catalog (db_version
// 155), infers edit->source pairs from filename convention (the catalog
// itself stores no relational lineage to read directly -- see
// internal/luminar/query.go), resolves both endpoints against a local node
// index (see internal/nodeindex's doc comment for why: there is no
// agent-reachable lookup-by-path endpoint on branchDAM today), and emits
// EVENT_EDGE_ATTACHED at tier 2 / confidence 0.89 -- deliberately below
// branchDAM's tier-2 auto-accept threshold, so every edge lands in the human
// audit queue rather than auto-committing a filename-inferred pairing.
func runLuminarSyncCmd(args []string) int {
	fs := flag.NewFlagSet("luminar-sync", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to branchdam-agent config file (default: ./config.yaml if present, else the per-user config directory)")
	catalogPath := fs.String("catalog", "", "path to Luminar's catalog file (falls back to integrations.luminar.catalogPath in config; required from one source or the other, unless -dump-schema)")
	nodeIndexPath := fs.String("node-index", "", "path to the node-index JSON file mapping file paths to nodeUuids (falls back to integrations.nodeIndexPath in config; required from one source or the other, unless -dump-schema)")
	queryFile := fs.String("query-file", "", "path to a SQL file overriding the built-in catalog row-extraction query -- CLI-only, deliberately not config-driven (see docs/luminar-catalog.md)")
	derivativeSuffixes := fs.String("derivative-suffixes", "", "comma-separated derivative-filename suffixes overriding the built-in default (_upscale,_panorama); empty means use the default, not match nothing -- CLI-only, deliberately not config-driven")
	dryRun := fs.Bool("dry-run", false, "resolve and log what would be emitted without contacting the server")
	dumpSchema := fs.Bool("dump-schema", false, "print the catalog's sqlite_master schema and exit, instead of syncing")
	timeout := fs.Duration("timeout", 30*time.Second, "overall command timeout")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// explicitlySet records which flags the operator actually passed on
	// argv, as opposed to a flag's own zero value -- Visit (unlike ranging
	// fs.Args or checking *catalogPath/*nodeIndexPath directly) is the only
	// way to distinguish "not passed" from "passed the zero value
	// explicitly". Used below for -catalog/-node-index's config fallback.
	//
	// -dry-run deliberately gets NO config fallback and is NOT in this
	// set's use -- see the comment at its check below for why: unlike
	// -catalog/-node-index, config.Load cannot distinguish "the operator
	// configured dry-run" from "nobody has touched this section at all",
	// so falling back here would silently turn every already-scripted
	// `luminar-sync -catalog X -node-index Y` invocation into a no-op that
	// still exits 0.
	explicitlySet := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicitlySet[f.Name] = true })

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: resolve config path: %v\n", err)
		return 1
	}
	cfg, cfgErr := config.Load(resolvedPath)
	// cfgErr is handled further down, once we know whether this run
	// actually needs server credentials from it (a dry run doesn't).

	if !explicitlySet["catalog"] && cfg.Integrations.Luminar.CatalogPath != "" {
		*catalogPath = cfg.Integrations.Luminar.CatalogPath
	}
	if !explicitlySet["node-index"] && cfg.Integrations.NodeIndexPath != "" {
		*nodeIndexPath = cfg.Integrations.NodeIndexPath
	}

	if *catalogPath == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent luminar-sync: -catalog is required (or integrations.luminar.catalogPath in config)")
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	catalog, err := luminar.Open(ctx, *catalogPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: %v\n", err)
		return 1
	}
	defer func() { _ = catalog.Close() }()

	if *dumpSchema {
		return runDumpSchema(ctx, os.Stdout, catalog)
	}

	if *nodeIndexPath == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent luminar-sync: -node-index is required (or integrations.nodeIndexPath in config, unless -dump-schema)")
		return 2
	}

	query := ""
	if *queryFile != "" {
		q, err := luminar.LoadQueryFile(*queryFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: %v\n", err)
			return 1
		}
		query = q
	}

	index, err := nodeindex.Load(*nodeIndexPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: %v\n", err)
		return 1
	}

	if cfgErr != nil {
		if !*dryRun {
			fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: load config %q: %v\n", resolvedPath, cfgErr)
			return 1
		}
		// -dry-run never contacts the server, so a missing/unreadable
		// config is a warning, not a hard failure -- an operator should be
		// able to try a dry run against a catalog before config.yaml even
		// exists.
		fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: warning: load config %q: %v (continuing, -dry-run needs no server config)\n", resolvedPath, cfgErr)
	} else if problems := cfg.Validate(); len(problems) > 0 {
		// Printed as warnings, never fatal by themselves -- Validate()
		// checks the WHOLE config (ingest.*, prune.*, offline.*, ...), most
		// of which luminar-sync doesn't touch at all. Mirrors
		// runTrayCmd's own startup gate: only a server.*-prefixed problem
		// is actually fatal here (checked explicitly below via
		// cfg.Server.APIKey), since that's the one thing this command
		// can't proceed without once it needs to contact the server.
		for _, p := range problems {
			fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: warning: config problem: %s\n", p)
		}
	}
	// -dry-run deliberately reads only the flag's own value (never a
	// config fallback): the CLI has always defaulted to live (edges are
	// emitted unless -dry-run is passed), and config.Load returns
	// defaultConfig()'s DryRun: true both when config.yaml is missing and
	// when it parses with no integrations: block at all -- there is no way
	// to distinguish "the operator opted into dry-run" from "nobody has
	// configured this integration" at that point. Falling back here would
	// silently turn every already-scripted
	// `luminar-sync -catalog X -node-index Y` invocation (cron jobs,
	// CI, etc.) into a no-op that still exits 0. integrations.luminar.dryRun
	// governs the TRAY's own timer-driven sync only, once that lands.
	if !*dryRun && cfg.Server.APIKey == "" {
		fmt.Fprintln(os.Stderr, "branchdam-agent luminar-sync: server.apiKey is empty in config (use -dry-run to skip the server entirely)")
		return 2
	}

	var client luminar.EdgeAttacher
	if !*dryRun {
		client = branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey)
	}

	var suffixes []string
	if *derivativeSuffixes != "" {
		for _, s := range strings.Split(*derivativeSuffixes, ",") {
			if s = strings.TrimSpace(s); s != "" {
				suffixes = append(suffixes, s)
			}
		}
	}

	syncer := &luminar.Syncer{
		Catalog:            catalog,
		Index:              index,
		Client:             client,
		AgentID:            cfg.AgentID,
		CatalogPath:        *catalogPath,
		Query:              query,
		DerivativeSuffixes: suffixes,
		DryRun:             *dryRun,
	}

	stats, err := syncer.Sync(ctx)
	fmt.Printf(
		"luminar-sync: %d pair(s) found (%d ambiguous, %d no source, %d unresolvable path), %d emitted, %d skipped (source unresolved), %d skipped (edit unresolved), %d error(s)\n",
		stats.PairsFound, stats.Ambiguous, stats.NoSourceInCatalog, stats.PathUnresolvable,
		stats.Emitted, stats.SourceUnresolved, stats.EditUnresolved, stats.Errors,
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: %v\n", err)
		return 1
	}
	if stats.Errors > 0 {
		return 1
	}
	return 0
}

func runDumpSchema(ctx context.Context, w io.Writer, catalog *luminar.Catalog) int {
	objs, err := catalog.DumpSchema(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: %v\n", err)
		return 1
	}
	for _, o := range objs {
		if _, err := fmt.Fprintf(w, "-- %s: %s\n%s\n\n", o.Type, o.Name, o.SQL); err != nil {
			fmt.Fprintf(os.Stderr, "branchdam-agent luminar-sync: write schema dump: %v\n", err)
			return 1
		}
	}
	return 0
}
