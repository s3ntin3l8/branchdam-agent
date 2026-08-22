# CLAUDE.md — Go HTTP Template

A Go net/http backend starter wired to the centralized CI/CD in
[`s3ntin3l8/.github`](https://github.com/s3ntin3l8/.github). If you are an AI agent
or developer working in a repo created from this template, read this first.

## First steps after creating a repo from this template

1. Rename the placeholders: `module` in `go.mod`, the `# Project Name` title in
   `README.md`, and the `module` path across `.go` files.
2. `make install-hooks` — installs pre-commit and pre-push hooks.
3. `make build` — verify everything compiles.
4. Decide your CI coverage floor: `ci-cd.yml` ships `coverage-fail-under: '0'`
   (a starter floor) — **ratchet it up** as you add real code.

## Commands (Makefile)

| Command | Does |
|---------|------|
| `make install-hooks` | Install pre-commit + pre-push hooks. |
| `make test` | Run Go tests with race detection and coverage. |
| `make lint` | Run pre-commit on all files (golangci-lint + go vet + detect-secrets). |
| `make fmt` | Format Go code with gofmt (and goimports if available). |
| `make vet` | Run go vet. |
| `make tidy` | Run go mod tidy. |
| `make vulncheck` | Run govulncheck for known vulnerabilities. |
| `make build` | Build all packages. |
| `make clean` | Remove build artifacts and test caches. |

## Layout

| Path | Responsibility |
|---|---|
| `cmd/server/main.go` | Entrypoint: flag parsing, config loading, `signal.NotifyContext`-driven graceful shutdown |
| `cmd/server/health.go` | The `-healthcheck` self-probe used by the Dockerfile's `HEALTHCHECK` (normalizes a wildcard listen address like `0.0.0.0:8080` to a loopback URL before probing) |
| `internal/config/` | YAML config loader with `${VAR}` environment expansion (no `:-default` support — see `config.example.yaml`'s comment) |
| `internal/httpapi/` | `net/http` server setup: route mux, security headers, panic recovery, request logging, `/health` |
| `config.example.yaml` | Reference config with `${VAR}` placeholders |
| `Dockerfile` | Multi-stage build → distroless non-root runtime with `HEALTHCHECK`, version stamped via `-ldflags -X main.version=...` |
| `.github/workflows/` | Thin callers of the reusable workflows in `s3ntin3l8/.github` |
| `.editorconfig` | Shared editor settings (LF, UTF-8, final newline; tabs for Go) |
| `.claude/` | `settings.json` + `hooks/session-start.sh`: a SessionStart hook that installs Go deps and tooling (pre-commit, golangci-lint, govulncheck) so [Claude Code on the web](https://code.claude.com/docs/en/claude-code-on-the-web) sessions can build, test, and lint. Runs only in the remote env |

## Key invariants

Rules a change to this repo should not silently break, with the reasoning behind each — add to
this list as the codebase grows past "starter template." It's intentionally short today:

- **`config.example.yaml`'s values must be literal, not `${VAR:-default}`.** `expandEnv`
  (`internal/config/config.go`) captures everything between `${` and `}` as one literal
  environment-variable name; it does not parse a `:-default` separator. A value like
  `"${LISTEN_ADDR:-:8080}"` therefore becomes the *literal string* `"${LISTEN_ADDR:-:8080}"`
  whenever `LISTEN_ADDR` is unset, not the intended default — this was a real bug in this file
  once. Use `${VAR}` only for values genuinely meant to be environment-driven, and match
  `defaultConfig()`'s Go-side defaults for the example's literal values.

## CI/CD — uses centralized reusable workflows

Workflows here are **callers** of `s3ntin3l8/.github/.github/workflows/*.yml@main`:
`ci-cd.yml` (ci-go + docker-publish), `codeql.yml`, `dependency-review.yml`,
`release-please.yml`, `cleanup-ghcr.yml`.

**The #1 thing to get right:** a caller job that invokes a reusable workflow needing
write scopes **must declare a `permissions:` block** — the default `GITHUB_TOKEN`
is read-only and the run otherwise fails at startup with zero jobs. The caller's
grant must cover **every** scope the reusable workflow's jobs declare, or the run
fails at startup. `build-docker` needs `contents: read` + `packages: write` +
`id-token: write` (the last for keyless image signing); `codeql` needs
`security-events: write`;
`release-please` needs `contents: write` + `pull-requests: write`. See the
`s3ntin3l8/.github` README for the full table.

`ci-go` reads the Go version from `go.mod`, runs gofmt, go vet, go build, go test
-race with coverage, and govulncheck. The `pre-build-commands` input is available
for project-specific setup (most commonly stubbing `//go:embed` assets).

> **Codecov TODO:** coverage upload requires a `CODECOV_TOKEN` repo secret and the
> repo onboarded on [codecov.io](https://about.codecov.io/) before results/badges
> show. The workflow runs the upload unconditionally; it just no-ops without the token.

## Conventions

- **Go 1.26+, stdlib-first.** `net/http` router, no framework dependency.
- **Conventional Commits** — Release Please cuts versions/changelogs from them.
- **Linting enforced** by golangci-lint and go vet (config in `.pre-commit-config.yaml`);
  run `make lint` before pushing (the pre-push hook runs govulncheck).
- **Secrets:** never commit real credentials; `detect-secrets` runs in pre-commit
  and CI against `.secrets.baseline` (regenerate with
  `detect-secrets scan > .secrets.baseline` after vetting new detections).

## Documentation map

- `README.md` — setup and usage instructions for whatever you build from this template.
- `CLAUDE.md` (this file) — the live contract for how the repo is wired: layout, invariants,
  CI/CD conventions. Keep it in sync with the code, not with what the code used to do.

Add real docs here as the project grows past what these two files can hold on their own (e.g. an
API contract, an architecture decision log, an operational runbook) — this section exists so
that pattern has an obvious place to start rather than being invented under time pressure later.
