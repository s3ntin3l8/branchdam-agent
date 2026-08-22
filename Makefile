.DEFAULT_GOAL := help
.PHONY: help install-hooks test lint fmt vet tidy vulncheck build clean check

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

check: build vet test ## One-shot pre-PR gate: build + vet + test (does not require pre-commit -- see `lint`)

clean: ## Remove build artifacts and caches
	rm -f coverage.txt
	go clean -testcache
