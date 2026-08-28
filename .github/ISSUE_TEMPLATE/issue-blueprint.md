---
name: Issue Blueprint
about: File an issue following the branchdam-agent context and scope blueprint.
title: "type(scope): short description"
labels: ""
assignees: ""
---

## Context

<!-- Problem or spec pillar, current behaviour, code references. -->

```go
// Code snippet showing the affected area
```

## Scope

<!-- What this issue changes. One issue = one PR. -->

## Out of scope

<!-- What deliberately isn't covered, so it doesn't get scope-crept in review. -->

## Acceptance criteria

- [ ] <!-- Requirement 1 -->
- [ ] Docs updated (`docs/*.md`, `CLAUDE.md`) if behaviour or invariants changed
- [ ] Test coverage added/modified
- [ ] `make lint && make check` green
- [ ] `make build-windows` / `make build-darwin` still pass if a platform-specific path changed

## Notes

Manual: true

Blocked by: #
Branch off `origin/main`. One PR.
