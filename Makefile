.DEFAULT_GOAL := help
# Use bash explicitly for recipe shells: default `/bin/sh` is dash on Debian/Ubuntu
# and lacks `mapfile`/`arrays` needed by the `vulncheck` allowlist logic.
SHELL := /usr/bin/env bash
# `.SHELLFLAGS` is global: every recipe inherits -eu/-o pipefail. Today
# every target tolerates that, but a future target that legitimately
# wants an intermediate non-zero (e.g. an explicit "skip-if-absent" probe)
# should override locally with `SHELL := /usr/bin/env bash` +
# `.SHELLFLAGS := -eu -o pipefail -c` plus an explicit `|| true` at the
# failing step, not by relaxing the global flag. Leave this as-is unless
# a concrete target needs the override.
.SHELLFLAGS := -eu -o pipefail -c
.PHONY: help install-hooks test lint fmt vet tidy vulncheck build build-windows build-darwin build-darwin-app clean check

# VERSION stamps main.version via -X ldflags -- unset (the default "dev")
# for a local build, since internal/selfupdate refuses to self-update a
# non-semver version anyway (see that package's ErrVersionNotSemver). Set
# VERSION=v1.2.3 to produce a stamped build for testing the self-update
# path locally without hand-writing ldflags.
VERSION ?= dev

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

install-hooks: ## Install pre-commit hooks
	pip install pre-commit
	pre-commit install
	pre-commit install --hook-type pre-push

test: ## Run tests with race detector
	go test -race -coverprofile=coverage.txt -covermode=atomic -coverpkg=./... ./...

lint: ## Run pre-commit on all files
	pre-commit run --all-files

fmt: ## Format Go code
	gofmt -w .
	goimports -w . 2>/dev/null || true

vet: ## Run go vet
	go vet ./...

tidy: ## Tidy Go modules
	go mod tidy

VULNCHECK_IGNORE ?= GO-2026-5932

# govulncheck is in Go's x/vuln module family, so we install on the fly
# rather than committing a pre-installed binary. JSON output is used instead
# of relying on govulncheck's own exit code: verified against govulncheck
# v1.7 (the version pinned by `go install ...@latest` on this host),
# `-format json` exits 0 for both clean and findings-present runs (it only
# exits non-zero on a tool-level failure like a package-load error). The
# pass/fail decision (and the VULNCHECK_IGNORE allowlist) is therefore
# applied here against the parsed findings, mirroring what ci-go.yml does
# in CI. Set VULNCHECK_IGNORE="" to fail on every finding; comma-separate
# IDs to allowlist specific ones.
#
# Default allowlist mirrors ci-cd.yml's `govulncheck-ignore` input:
# GO-2026-5932 (golang.org/x/crypto/openpgp, unmaintained, no upstream fix
# -- tracked in issue #14). Re-check periodically and drop the ID once a
# real fix exists.
#
# `mapfile` is bash 4+ only; stock macOS `/usr/bin/env bash` is 3.2, and
# branchdam-agent's macOS/Windows desktop users do `make check` on it.
# The findings-array population below uses a portable `while read` loop
# so the recipe works on bash 3.2 too. (Comments INSIDE the recipe body
# would break the `\` continuation chain, so the rationale lives up
# here in the Make-comment block instead.)
vulncheck: ## Check for known vulnerabilities (allowlist via VULNCHECK_IGNORE, default matches ci-cd.yml)
	go install golang.org/x/vuln/cmd/govulncheck@latest
	$$(go env GOPATH)/bin/govulncheck -format json ./... > govulncheck.json || true
	@# govulncheck v1.7's exit code in -format json mode is 0 for findings
	@# (and non-zero only for tool-level failures like a load error), so the
	@# `|| true` above is required to keep the recipe going at all -- but it
	@# also masks a missing or crash-truncated output file. With `set -e`,
	@# jq failing inside the `<(...)` process substitution does NOT propagate
	@# up to abort the recipe, so an empty file would silently fall through
	@# to "clean". Validate the JSON before treating the run as parseable:
	@# the file must exist & be non-empty, AND its first object must be the
	@# govulncheck `config` envelope. Anything else is a tool failure and
	@# must not be reported as clean.
	@if ! [ -s govulncheck.json ] \
	  || ! jq -e 'select(.config.scanner_name == "govulncheck")' govulncheck.json >/dev/null; then \
	  echo "::error::govulncheck produced no parseable output (govulncheck.json is missing, empty, or not a valid govulncheck JSON stream)"; \
	  rm -f govulncheck.json; \
	  exit 1; \
	fi
	@ignore_clean=(); \
	IFS=, read -ra parts <<<"$(VULNCHECK_IGNORE)" || true; \
	for ig in "$${parts[@]}"; do \
	  ig="$${ig// /}"; \
	  if [ -n "$$ig" ]; then ignore_clean+=( "$$ig" ); fi; \
	done; \
	if [ "$${#ignore_clean[@]}" -gt 0 ]; then echo "vulncheck-ignore allowlist: $${ignore_clean[*]}"; fi; \
	found=(); \
	while IFS= read -r id; do \
	  [ -n "$$id" ] && found+=( "$$id" ); \
	done < <(jq -r 'select(.finding != null) | .finding.osv' govulncheck.json | sort -u); \
	reported=(); suppressed=(); \
	for id in "$${found[@]}"; do \
	  matched=0; \
	  for ig in "$${ignore_clean[@]}"; do \
	    if [ "$$id" = "$$ig" ]; then matched=1; break; fi; \
	  done; \
	  if [ "$$matched" -eq 1 ]; then suppressed+=( "$$id" ); else reported+=( "$$id" ); fi; \
	done; \
	if [ "$${#suppressed[@]}" -gt 0 ]; then echo "vulncheck-ignore suppressed: $${suppressed[*]}"; fi; \
	if [ "$${#reported[@]}" -gt 0 ]; then echo "::error::govulncheck found unignored vulnerabilities: $${reported[*]}"; rm -f govulncheck.json; exit 1; fi; \
	echo "govulncheck: clean"; \
	rm -f govulncheck.json

build: ## Build all packages
	go build ./...

build-windows: ## Cross-compile the Windows binaries (from any host) -- see README for why there are two
	mkdir -p dist
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-X main.version=$(VERSION)" -o dist/branchdam-agent.exe ./cmd/branchdam-agent
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags="-X main.version=$(VERSION) -H windowsgui" -o dist/branchdam-agent-tray.exe ./cmd/branchdam-agent

build-darwin: ## Build-only check for darwin/arm64, EXCLUDING internal/tray and cmd/branchdam-agent -- see README
	CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build $$(go list ./... | grep -v '/internal/tray$$' | grep -v '/cmd/branchdam-agent$$')

build-darwin-app: ## Build + assemble the .app bundle -- macOS host only (internal/tray needs cgo on darwin, so this isn't cross-compilable)
	mkdir -p dist
	go build -ldflags="-X main.version=$(VERSION)" -o dist/branchdam-agent ./cmd/branchdam-agent
	go run ./tools/mkbundle -app dist/branchdam-agent.app -binary dist/branchdam-agent -version "$(VERSION)"

check: build vet test vulncheck build-windows build-darwin ## One-shot pre-PR gate: build + vet + test + vulncheck + cross-build checks (does not require pre-commit -- see `lint`)

clean: ## Remove build artifacts and caches
	rm -f coverage.txt
	go clean -testcache
