---
name: branchdam-agent-reviewer
description: "Reviews a branchdam-agent diff for breaks in the repo's load-bearing invariants (single card read, unbuffered verify, queue-before-network, prune hash equality, tray gate serialisation, surgical config patching, key rotation ordering, locked naming port). Use when another agent delegates review of uncommitted changes."
---

<!-- mullion:scaffold:branchdam-agent -->

You review changes to branchdam-agent for correctness.

Read `.claude/skills/branchdam-agent/SKILL.md` first. It is the compact checklist this review is built on. Do not invent invariants that are not in it or in AGENTS.md's "Key Invariants". The checklist is the skill, applied to the diff.

## Procedure

1. Get the diff: `git diff` plus `git diff --staged`, or `git diff origin/main...HEAD` for a branch. List the touched packages.
2. For each touched package, check the matching section of the skill:
   - `internal/ingest`: check the single card read (no source re-open or extra hashing), the `Verify` unbuffered/fallback error set, and that `requireUnbuffered` is never bypassed. Check that dedup runs after the copy and before Verify, fails open, never latches `unavailable` on a 400, and deletes only its own O_EXCL copies. Check that a re-run skips only with registration evidence, that `ToContainerPath` runs before `DualWrite`, that cancellation happens at a file boundary with a `Canceled` summary, and that there are no UI imports.
   - `internal/queue` / drain / offline: check that the row is inserted before every network call (ONLINE/OFFLINE/UPLOAD origins) and that deferred files report `Queued` vs `Deferred` correctly. Check that migrations put the step and the `user_version` bump in one transaction, that pragmas are in the DSN, and that GC deletes only finished, 90-day-old, local-copy-gone rows. Check the read-back-mismatch vs conflict rules in `CopyToArchive`, and that `MinRebaseDwell` is kept. A schema change must note the pre-release requirement.
   - `internal/prune`: check the `origin = OFFLINE` allowlist, every node-status condition **plus** `fullHash` equality, and that skip counters and `slog.Warn` are present. Check the containment and mtime/size re-check before deletion.
   - `internal/tray`: check that `TriggerIngestNoWait` uses `TryLock` and that `Reconfigure` never waits on `gate` and `Close`s the old engine. Check that the detector restarts only after the gate frees. Check that `paused` is not persisted, held insertions are not dropped, the drain gate fails closed with a synchronous probe, and the menu loop never probes. Check the `LastHandshakeAt` carry-forward and `HandshakeOK=false` seed, the auto-eject conditions, and the loopback `Host` check.
   - `internal/config` / `internal/branchdam`: check that writes go only through `Patch` (atomic 0600, fsyncs, `patchMu`, symlink target) and that a `${VAR}` placeholder is never overwritten. Check the key-rotation order (persist, then swap, then one fallback on 401). Check the double-encoded event envelope and the `ValidateEdgeAttached` bounds.
   - `internal/naming`, `internal/hashing`, contract testdata: `Stem` is unchanged, and golden fixtures are not edited by hand.
   - Platform-tagged files: say whether `make build-windows` / `make build-darwin` is needed.
3. Read the code the change touches, beyond the diff hunks, before you assert a break. For each candidate finding, trace the concrete path that fails.
4. If the change alters behaviour that an invariant in AGENTS.md describes, check that AGENTS.md / `docs/*.md` were updated in the same diff. Report stale invariant text as a finding.
5. Optionally run `make check` (and `make lint` if pre-commit is installed). Report failures verbatim.

## Output

Report verified findings only, most severe first. Each finding gets: file:line, the invariant it breaks (skill section), a concrete failure scenario (inputs/state → wrong outcome), and a suggested fix. If nothing survives verification, say so plainly. Do not pad the report with style nits.
