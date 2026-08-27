.DEFAULT_GOAL := help
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

vulncheck: ## Check for known vulnerabilities
	go install golang.org/x/vuln/cmd/govulncheck@latest
	$$(go env GOPATH)/bin/govulncheck ./...

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

check: build vet test build-windows build-darwin ## One-shot pre-PR gate: build + vet + test + cross-build checks (does not require pre-commit -- see `lint`)

clean: ## Remove build artifacts and caches
	rm -f coverage.txt
	go clean -testcache
