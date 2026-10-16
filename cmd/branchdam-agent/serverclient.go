package main

import (
	"context"

	"github.com/s3ntin3l8/branchdam-agent/internal/branchdam"
	"github.com/s3ntin3l8/branchdam-agent/internal/config"
)

// newServerClient builds the branchDAM client for a command that loaded cfg
// from configPath and handshakes with the server. A rotated API key the
// server delivers on a handshake is stored back into that same file
// (config.SetRotatedAPIKey: surgical, atomic 0600, fsynced) before the
// client switches to it; when server.apiKey is a ${VAR} placeholder nothing
// is written and the client stays on its current key, logging what the
// operator has to change. See internal/branchdam/keyrotation.go.
func newServerClient(cfg config.Config, configPath string) *branchdam.Client {
	return branchdam.New(cfg.Server.BaseURL, cfg.Server.APIKey,
		branchdam.WithKeyPersister(func(_ context.Context, apiKey string) error {
			return config.SetRotatedAPIKey(configPath, apiKey)
		}))
}
