## Summary

<!-- Describe what changed, why, and any context or subproblems. -->

### Changes made:

<!-- Numbered list of changes per file/component. E.g.,
1. **internal/ingest/dualwrite.go**: Added a third concurrent writer for the scratch mirror.
-->

### Key design decisions:

<!-- Rationale for non-obvious choices. -->

## Test plan / Verification

- [ ] `make lint` (pre-commit: golangci-lint, gofmt, go vet, detect-secrets)
- [ ] `make check` — build + vet + `go test -race` (coverage)
- [ ] `make build-windows` / `make build-darwin` if this touches a platform-specific path
      (`internal/tray`, `internal/appbundle`, `internal/autostart`, ingest verify's
      O_DIRECT/F_NOCACHE/FILE_FLAG_NO_BUFFERING branches)
- [ ] `govulncheck ./...` clean, or any new finding is either fixed (dependency bump) or
      added to `GOVULNCHECK_IGNORE` in CI with a comment explaining why it can't be fixed
- [ ] Ingest/queue changes: intent is still persisted to `queue.db` *before* any network call
      (Key Invariant 3 — offline queue safety)
- [ ] Verify/hashing changes: still a single card read (Key Invariant 1 — no re-read to hash)
- [ ] Config changes: written through the surgical `yaml.Node` patcher (`internal/config/patch.go`),
      not a naive marshal-and-overwrite (Key Invariant 6 — preserves comments and `${VAR}` placeholders)
- [ ] Tray/ingest concurrency changes: still serialized behind `Runner.TriggerIngest`'s `gate`
      (Key Invariant 9)

<!-- Note: `make lint` runs golangci-lint as part of pre-commit here (unlike the main
     branchDAM repo, where golangci-lint is a separate target) -- see CONTRIBUTING.md. -->

Closes #<!-- Issue Number -->

<!--
PR title must use a Conventional Commits prefix (feat:, fix:, chore:, docs:, ...).
This repo squash-merges PRs and Release Please parses the PR title, not the
individual commits -- an unprefixed title silently drops from the changelog.

See CONTRIBUTING.md for the full pre-PR checklist and branch-protection details.
-->
