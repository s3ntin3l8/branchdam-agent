# Go HTTP Template

A minimal Go backend template using `net/http` (stdlib), structured config, and
full CI/CD via reusable workflows from
[`s3ntin3l8/.github`](https://github.com/s3ntin3l8/.github).

## Quick Start

### 1. Installation

```sh
make install-hooks   # set up pre-commit + pre-push hooks
```

### 2. Development

```sh
make build           # compile all packages
make test            # run tests with race detection
```

### 3. Run the server

```sh
cp config.example.yaml config.yaml
go run ./cmd/server -config ./config.yaml
```

The server listens on the address from `config.yaml` (default `:8080`) and
exposes a `/health` endpoint.

## Commands

| Command | Does |
|---------|------|
| `make install-hooks` | Install pre-commit + pre-push hooks. |
| `make test` | Run Go tests with race detection and coverage. |
| `make lint` | Run pre-commit on all files. |
| `make fmt` | Format Go code. |
| `make vet` | Run go vet. |
| `make tidy` | Run go mod tidy. |
| `make vulncheck` | Check for known vulnerabilities. |
| `make build` | Build all packages. |
| `make clean` | Remove build artifacts and caches. |

## Security

- This project follows the [s3ntin3l8 Global Security Policy](https://github.com/s3ntin3l8/.github/blob/main/SECURITY.md).
- Security scans (CodeQL) and dependency reviews are automated in the CI pipeline.
- `detect-secrets` runs in pre-commit and CI against `.secrets.baseline`.

## Releases

Releases are automated via [Release Please](https://github.com/googleapis/release-please).
Use [Conventional Commits](https://www.conventionalcommits.org/) to trigger version bumps.

## License

AGPL-3.0
