# AGENTS.md — branchdam-agent

<!-- mullion:briefing:start -->

Workstation companion agent for branchDAM. Handles SD card ingestion, dual-copy
verified storage writes, offline queueing, and catalog sync. Go binary
(cross-platform: Linux, Windows, macOS) with system tray UI.

Key commands: `make check` (build + vet + test), `make lint` (pre-commit).

Critical invariants: streaming dual-write (one read), cache-defeating verify
(unbuffered I/O), offline queue persist-before-network. Full details in
`CLAUDE.md`.

<!-- mullion:briefing:end -->
