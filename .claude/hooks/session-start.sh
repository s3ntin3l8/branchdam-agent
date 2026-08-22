#!/bin/bash
# SessionStart hook: prepare a Claude Code on the web session so that
# `make build`, `make test`, and `make lint` work out of the box.
# Idempotent and non-interactive — safe to run multiple times.
set -euo pipefail

# Only run in the remote (web) environment; local sessions manage their own setup.
if [ "${CLAUDE_CODE_REMOTE:-}" != "true" ]; then
  exit 0
fi

cd "${CLAUDE_PROJECT_DIR:-.}"

# Go module dependencies (cached into the container image after first run).
go mod download

# pre-commit drives `make lint` (golangci-lint + go vet + detect-secrets).
if ! command -v pre-commit >/dev/null 2>&1; then
  pip install --quiet pre-commit
fi

# golangci-lint: install the version pinned in .pre-commit-config.yaml if absent.
if ! command -v golangci-lint >/dev/null 2>&1; then
  go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
fi

# govulncheck is run by `make vulncheck` and the pre-push hook.
if ! command -v govulncheck >/dev/null 2>&1; then
  go install golang.org/x/vuln/cmd/govulncheck@latest
fi

# Ensure the Go tool bin dir is on PATH for the rest of the session.
echo "export PATH=\"$(go env GOPATH)/bin:\$PATH\"" >> "${CLAUDE_ENV_FILE:-/dev/null}"
