# CLAUDE.md — branchdam-agent

Guidance for Claude Code (claude.ai/code) working in `branchdam-agent`.

## Guidelines

- **PR & Issue Templates:** Fill [`.github/pull_request_template.md`](.github/pull_request_template.md) and [`.github/ISSUE_TEMPLATE/issue-blueprint.md`](.github/ISSUE_TEMPLATE/issue-blueprint.md). See [`CONTRIBUTING.md`](CONTRIBUTING.md) for branch protection and checklist.
- **Review Thread Resolution:** Hermes/human review threads require two API calls to resolve:
  ```sh
  # 1. Reply to inline comment (REST)
  gh api repos/s3ntin3l8/branchdam-agent/pulls/<PR>/comments/<comment_id>/replies -f body="Fixed in <sha>"
  # 2. Resolve thread (GraphQL)
  gh api graphql -f query="mutation { resolveReviewThread(input: {threadId: \"<thread_id>\"}) { thread { isResolved } } }"
  ```

## Commands

```sh
# Pre-PR Gates & Testing
make check           # build + vet + test (race detector + coverage)
make lint            # pre-commit run --all-files (golangci-lint + vet + detect-secrets)

# Cross-compilation Builds
make build-windows   # Builds dist/branchdam-agent.exe and dist/branchdam-agent-tray.exe
make build-darwin    # Build-only check for darwin/arm64
make build-darwin-app# Assembles macOS .app bundle via tools/mkbundle
```

## Package Responsibilities

| Package | Responsibility |
|---|---|
| `cmd/branchdam-agent` | Subcommand dispatch (`preflight`, `ingest`, `tray`, `update`, `prune`, `luminar-sync`, `init`, hidden `dialog`) |
| `internal/branchdam` | REST client for branchDAM `/api/v1/agent/*` contract (`hello`, `handshake`, `events`, `rebase`, `node-status`) |
| `internal/ingest` | DualWrite (streaming multi-writer), cache-defeating Verify, EXIF/SRT metadata extraction, naming engine |
| `internal/queue` | Local SQLite queue (`queue.db`) persisting ingest records and retry states for offline field operation |
| `internal/tray` | Menu bar / system tray companion (Windows/macOS) with loopback HTTP status server (`127.0.0.1:38080`, `config.DefaultStatusAddr`) |
| `internal/selfupdate` | Checksum-validated self-update (`go-selfupdate`) with atomic apply and instant rollback |
| `internal/luminar` | Read-only Luminar Neo catalog parser (schema verified against `db_version 155`) generating Tier-2 lineage edges from filename-inferred pairing -- see `docs/luminar-catalog.md` |
| `internal/resolvehook` | Detects and installs DaVinci Resolve's render hook into its `Scripts/Utility` folder (`CandidateDirs`/`Detect`/`Install`, atomic temp-then-rename write) |
| `internal/nodeindex` | File path to `nodeUuid` lookup resolver |
| `internal/hashing` | Ported xxHash64 `FastHash`, `StreamingFastHasher`, BLAKE3 `FullHash`, and `PerceptualHash` |
| `internal/config` | YAML config loader with environment expansion and surgical `yaml.Node` atomic patcher |
| `internal/exiftool` | Pooled `exiftool -stay_open` subprocess manager (`Pool`), shared by `internal/ingest`'s EXIF extraction and `internal/phash`'s RAW preview fallback |

## Key Invariants

1. **Streaming Dual-Write (`DualWrite`)**: Ingestion reads the camera card once, streaming bytes to both the local edit SSD and NAS archive simultaneously while computing xxHash64 and BLAKE3-256 in-flight. Never re-reads the card to compute hashes.
2. **Cache-Defeating Verification (`Verify`)**: Flushes (`fsync`), closes, and re-opens files unbuffered (`O_DIRECT` on Linux, `F_NOCACHE` on macOS, `FILE_FLAG_NO_BUFFERING` on Windows). If unbuffered open fails (e.g. tmpfs or CIFS), falls back to the buffered floor once at open time; never mid-stream.
3. **Offline Queue Safety (`queue.db`)**: Ingest intent is persisted in local SQLite before network calls. Offline drain enforces `MinRebaseDwell` (4s) to guarantee server-side event consumption before emitting `/rebase`.
4. **Path Translation (`ToContainerPath`)**: Agent payload paths are container-absolute, translated client-side via `pathMappings` (longest-prefix match) before dispatch.
5. **Double-Encoded Event Envelopes**: `/api/v1/agent/events` payload is a double-encoded JSON string; inner `evidenceJson` in `EVENT_EDGE_ATTACHED` is a raw JSON object.
6. **Surgical Config Patching (`patch.go`)**: Config writes mutate raw `yaml.Node` ASTs and write atomically (0600), preserving comments and unexpanded `${VAR}` placeholders.
7. **Client-Side Edge Validation**: `ValidateEdgeAttached` strictly enforces confidence `[0.50, 1.00]`, allowed relationship types, and rejects agent-supplied `reviewState` before POSTing.
8. **Prune Safety Checks**: `prune` only operates on `queue.db` items confirmed verified by `POST /api/v1/agent/node-status`. Re-verifies local containment and disk mtime/size before deletion.
9. **Tray Gate Serialization**: `Runner.TriggerIngest` serializes concurrent card insertions and manual clicks behind `gate`. Reconfiguration rebuilds components via `Runner.Reconfigure`.
