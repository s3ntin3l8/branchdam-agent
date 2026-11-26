package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
	"github.com/s3ntin3l8/branchdam-agent/internal/mcpserver"
)

func runMCPCmd(args []string) int {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	configPath := fs.String("config", "", "path to config file (default: ./config.yaml if present, else the per-user config directory)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	resolvedPath, err := config.ResolvePath(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent mcp: resolve config path: %v\n", err)
		return 1
	}
	cfg, err := config.Load(resolvedPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent mcp: load config %q: %v\n", resolvedPath, err)
		return 1
	}

	err = mcpserver.Run(context.Background(), mcpserver.Options{
		Version:    version,
		ConfigPath: resolvedPath,
		QueuePath:  cfg.Offline.QueueDBPath,
		StatusAddr: cfg.Tray.StatusAddrOrDefault(),
		ResolveStatusAddr: func() (string, error) {
			fresh, err := config.Load(resolvedPath)
			if err != nil {
				return "", err
			}
			return fresh.Tray.StatusAddrOrDefault(), nil
		},
		// Re-read on each call. agent_config already does, and a key or
		// pathMappings edit would otherwise leave preflight on the
		// startup snapshot.
		ResolveQueuePath: func() (string, error) {
			fresh, err := config.Load(resolvedPath)
			if err != nil {
				return "", err
			}
			return fresh.Offline.QueueDBPath, nil
		},
		Preflight: func(ctx context.Context) ([]mcpserver.CheckResult, bool, error) {
			fresh, err := config.Load(resolvedPath)
			if err != nil {
				return nil, false, err
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			var client helloCaller
			if fresh.Server.APIKey != "" {
				client = branchdam.New(fresh.Server.BaseURL, fresh.Server.APIKey)
			}
			checks, ok := runPreflightChecks(ctx, fresh, client, defaultLookPath, defaultRunVersion)
			return checks, ok, nil
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "branchdam-agent mcp: %v\n", err)
		return 1
	}
	return 0
}
