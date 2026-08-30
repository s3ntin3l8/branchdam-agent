# Contributing — branchdam-agent

## Branch protection

`main` requires these status checks: `test-go / lint-and-test`, `golangci-lint`, `CodeQL`, and
`dependency-review / dependency-review`. `strict` is `false` (branches need not be up to date
with `main` to merge) and `enforce_admins` is `false`. There are no required reviews (solo repo).

One expected wrinkle: release-please's own release PR never gets a CI check reported --
`release-please-action` opens/updates that PR using the default `GITHUB_TOKEN`, and GitHub's
Actions recursion guard means refs/PRs created by `GITHUB_TOKEN` don't trigger
`on: push`/`on: pull_request` workflows. With required status checks in place, that PR's merge
button reads as blocked/"Expected" forever, not just slow -- that's expected, not a bug. Merge
it via the "merge without waiting for requirements" path, which `enforce_admins: false` makes
available to the repo owner.

`required_conversation_resolution` is also on: every review thread (Hermes's or a human's) must
be replied to and resolved before a PR is mergeable, even with `enforce_admins: false`. See
[`AGENTS.md`](AGENTS.md)'s "Review thread resolution" guideline for the exact commands (thread
resolution is a GraphQL-only concept, not a `gh pr` verb).

## Automated review

Every non-draft PR gets an automated review from the `s3ntin3l8-hermes[bot]` GitHub App,
posted once on `opened` (or once on `ready_for_review` if the PR started as a draft). Ask for
another look at any point -- including after addressing feedback -- by commenting
`@s3ntin3l8-hermes Review` on the PR (or `@s3ntin3l8-hermes Triage` on an issue).

Because `required_conversation_resolution` is on, any inline comment Hermes (or a human
reviewer) attaches to a review thread blocks merge until that thread is replied to and resolved.
This does *not* gate on the review's overall verdict (`APPROVED`/`CHANGES_REQUESTED`) -- a
`CHANGES_REQUESTED` review whose findings live only in the summary body, with no inline comments,
does not block. See [`CLAUDE.md`](CLAUDE.md)'s branch-protection invariant for why that's the
deliberate trade-off (it keeps a Hermes outage from ever wedging a merge, since a human can
resolve threads without the bot).

Auto-review only runs for PRs from this repo, not forks -- `hermes.yml`'s `auto-review` job is
gated on `head.repo.full_name == github.repository`, since a `pull_request` event otherwise runs
a fork's own copy of the workflow file on the self-hosted runner (this repo is public). A
maintainer can still get a review on a fork PR by commenting `@s3ntin3l8-hermes Review` --
`issue_comment` always runs the default branch's copy of the workflow, in the base repo's
context, gated by commenter trust rather than fork origin.
