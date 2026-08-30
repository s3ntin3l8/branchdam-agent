# AGENTS.md — branchdam-agent

<!-- mullion:briefing:start -->

Workstation companion agent for branchDAM. Handles SD card ingestion, dual-copy
verified storage writes, offline queueing, and catalog sync. Go binary
(cross-platform: Linux, Windows, macOS) with system tray UI.

Key commands: `make check` (build + vet + test), `make lint` (pre-commit).

Critical invariants: streaming dual-write (one read), cache-defeating verify
(unbuffered I/O), offline queue persist-before-network. Full details in
`CLAUDE.md`.

## Review thread resolution

Every review thread (Hermes or human) must be replied to and resolved before
a PR is mergeable. This is a GraphQL-only concept, not a `gh pr` verb:

```sh
# 1. Reply to inline comment (REST)
gh api repos/s3ntin3l8/branchdam-agent/pulls/<PR>/comments/<comment_id>/replies -f body="Fixed in <sha>"
# 2. Resolve thread (GraphQL)
gh api graphql -f query="mutation { resolveReviewThread(input: {threadId: \"<thread_id>\"}) { thread { isResolved } } }"
```

<!-- mullion:briefing:end -->
