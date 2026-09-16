# Tray settings menu — `SettingsView`-missing field inventory

Issue: #110 (part of #92 E3 — Tray reliability & UX, audit finding F-14).
Source audit: `docs/audit/2026-09-01-comprehensive-audit.md` (2026-09-01 audit, Section 3, F-14).
The audit doc is gitignored (`/docs/audit/` in `.gitignore`) and is local-only working material; this document and the audit comment block in [`internal/tray/settings.go`](../internal/tray/settings.go) are the canonical record.

This is an **inventory** issue, not a code change. Each field below is a sub-task that graduates to `SettingsView` (and the matching `SettingsField` / `SettingsView` field, plus a `SetBool` / `SetInt` / `SetString` / `PromptAndSet` wiring where needed) as its M5 / E3 sub-issue ships. One small PR per field — this document and the audit comment block in `settings.go` are updated in the same PR that adds the field, so the two never disagree.

**Post-#211 note:** every "Graduated" row below originally pointed at a
tray systray menu item (`internal/tray/settingsmenu.go` /
`integrationsmenu.go`). Issue #211 (Track 3d's slimming half) removed those
menu items -- their config values now graduate into the Wails Settings
window instead (`cmd/branchdam-agent-ui/frontend/dist/app.js`'s
`FREE_TEXT_FIELDS`/`CHECKBOX_FIELDS`/`renderIntegrationBlock`), which reads
the identical `SettingsView` fields and writes through the identical
`SetBool`/`SetInt`/`SetString`/`SetIntegrationPath`/`SetIntegrationRewrites`
methods this doc already tracks. The `SettingsView` field, `SettingsField`
enum, and setter wiring this table cites are UNCHANGED -- only which UI
surface calls them moved, so the line numbers below pointing into
`settingsmenu.go`/`integrationsmenu.go` are stale (that code no longer
exists) but the graduation status itself still holds. `settingsmenu.go`
now owns only `Reload`/`OpenConfigFile`/`RevealConfigFolder` --
hand-edit-config affordances with no settings VALUE and therefore nothing
to graduate. `integrationsmenu.go` now owns only the per-integration status line, the
`integrations.<id>.timeoutSecs` field, and the "Sync now" action.
`timeoutSecs` is a genuine, not-yet-closed gap, not a deliberate one like
the fields in the "hand-edit only" list below: the Wails window's
`renderIntegrationBlock` has no sync-timeout control at all, so this is
the one per-integration value reachable ONLY from the tray now (everything
else moved to the window, or was already reachable from both).

## Fields the issue already enumerates

The issue body lists the graduation candidates in its acceptance-criteria checklist. The table below re-states each with (a) whether the field already exists in the `Config` struct today, (b) the M5 / E3 sub-issue that introduces the field if it does not, and (c) where in `SettingsView` / `SettingsField` the field lands once it does.

| Field | In `Config` today? | Sub-issue | Graduator |
|---|---|---|---|
| `ingest.autoEject: bool` (default `false`) | **Yes** (`IngestConfig.AutoEject`, `config.go:472`) | M5 #87 | Graduated: `SettingsView.AutoEject` + `SetBool("ingest.autoEject", ...)` (settings.go:239, settingsmenu.go:74) |
| `ingest.requireDCIM: bool` (default `false`) | **Yes** (`IngestConfig.RequireDCIM`, `config.go:432`) | M5 #81 | Graduated: `SettingsView.RequireDCIM` + `SetBool("ingest.requireDCIM", ...)` (settings.go:236, settingsmenu.go:72) |
| `ingest.allowedExtensions: []string` (default `[]`) | **Yes** (`IngestConfig.AllowedExtensions`, `config.go:448`) | M5 #81 | Graduated: `SettingsView.AllowedExtensions` + `FieldAllowedExtensions` (`settings.go:26`) + `validateStringChange("ingest.allowedExtensions", ...)` (`cmd/branchdam-agent/settings.go:294`). The single-value `PromptAndSet` was reused; no new list-editing dialog was needed because comma-separated input is parsed via `parseExtensionsList`. |
| `ingest.pauseUploadOnMetered: bool` (default `false`) | **Yes** (`IngestConfig.PauseUploadOnMetered`, `config.go:469`) | M5 #84 | Graduated: `SettingsView.PauseUploadOnMetered` + `SetBool("ingest.pauseUploadOnMetered", ...)` (settings.go:237, settingsmenu.go:73) |
| `ingest.autoImportPaths: []string` (default `[]`) | **Yes** (`IngestConfig.AutoImportPaths`, `config.go:458`) | M5 #79 | Graduated to a headless-only setting, **not** a `SettingsView` field or menu item -- there is no "auto-import paths" checkbox/list editor. The IngestGate's "Always auto-import" confirmation writes it via `SetStringSlice("ingest.autoImportPaths", ...)` (`cmd/branchdam-agent/tray.go:163`); the generic `POST /api/settings` route (`internal/tray/statusapi.go`) can also reach it, since that route dispatches on any dotted key. |
| `ingest.pathTemplate: string` (sync from `Handshake.NamingTemplate` at startup) | **Yes** (`IngestConfig.PathTemplate`, `config.go:416`) | M5 #86 | Graduated: existing `FieldNamingTemplate` / `SettingsView.NamingTemplate` pair (settings.go:16, 58) edits `cfg.Ingest.PathTemplate` via `PromptAndSet`. The Handshake sync (cmd/branchdam-agent/tray.go:392, settings.go:577) overwrites it at tray startup and on every `reload()` — operator edits survive only between Handshakes. |
| `tray.confirmDestructive: bool` (default `true`) | **Yes**, graduated | E3 #S2-14 (landed) | Graduated: `SettingsView.ConfirmDestructive` + `SetBool("tray.confirmDestructive", ...)` (`settingsmenu.go:68,126-131`) |
| Remove `cardRoots` from the "hand-edit only" list | **Yes**, graduated | M5 #78 (landed) | Graduated: `SettingsView` exposes `cardRoots` through `FieldCardRoots` (`settings.go:25`) and a "Watch folders…" menu item (`settingsmenu.go:145`); the menu edit hot-reloads via `Runner.ReconfigureDetector` (`tray.go:1262-1305`) — no process restart required. |
| `pathMappings` (`config.go:446-456`) | **Yes**, graduated | (not part of the original #110 checklist; graduated separately) | Graduated: `FieldPathMappings` (`settings.go:34`) + a "Path mappings…" menu item (`settingsmenu.go:90,167-168`), formatted as comma-separated `workstationPath:containerPath` pairs via `PromptAndSet`. The original hand-edit rationale (a wrong entry silently misroutes every event under that prefix) is mitigated by `PromptAndSet`'s validate-before-patch path plus `preflight`'s resolved-map print, not removed as a concern. |

## Fields deliberately hand-edit only (audit comment block also lists these)

These are in the audit comment block in [`internal/tray/settings.go`](../internal/tray/settings.go) and are not part of the issue's graduation pipeline:

- **`ingest.pollIntervalSecs` (config.go:413)** — low-frequency, restart-only knob. Not worth a menu slot. Operators adjust via `OpenConfigFile`.
- **`prune.*` (config.go:148-165)** — `enabled`, `minAgeHours`, `intervalMinutes`. Destructive subcommand gating; the hand-edit gate is the audit trail. A stray click on "Enable prune" with a wrong `minAgeHours` would be a one-step path to deleting verified archive mirrors.
- **`offline.*` (config.go:103-125)** — `queueDbPath`, `tier0ContainerRoot`, `drainIntervalSecs`. Changing the SQLite path or the staging container root mid-run breaks in-flight drain state; `drainIntervalSecs` is a tuning knob operators rarely touch.
- **`selfUpdate.repo` (config.go:211)** — a typo in the `owner/name` slug makes the next update check fetch from a non-existent or wrong repo. The hand-edit gate (plus a `selfupdate` log line that names the resolved repo on every check) is the safety net.
- **`tray.statusAddr` (config.go:190)** — loopback bind address. A non-loopback value exposes the unauthenticated status page on the network. Restart-only and intentionally hand-edit.
- **`ingest.exiftoolPath`** — overrides which exiftool binary `internal/exiftool.Pool` invokes; empty (the default) resolves `exiftool` through PATH. Added alongside the exiftool `-stay_open` pooling refactor (#104), not part of this issue's original enumeration. A rarely-touched operator override, not worth a menu slot.

## Graduation workflow

When an M5 / E3 sub-issue lands:

1. Land the M5 / E3 sub-issue's own PR (adding the field to `Config` + implementing the runtime side of the feature).
2. Open a follow-up PR that:
   - Adds the field to `SettingsView`.
   - Wires the matching `SetBool` / `SetInt` / `PromptAndSet` (or a new list-editing method for `AllowedExtensions` / `AutoImportPaths`).
   - Updates the audit comment block in `settings.go` (removes graduated fields, keeps the deliberately-hand-edit list intact).
   - Updates the table in this doc (moves the row from "to graduate" to the "deliberately hand-edit only" section, or to a new "graduated" section).
   - Closes the matching sub-task on #110.
3. `make check` must pass for the follow-up PR; `Settings.PromptAndSet` for any new free-text field is the same test surface as the existing `FieldServerBaseURL` / `FieldServerAPIKey` cases.

## Related context

- The existing `FieldNamingTemplate` / `SettingsView.NamingTemplate` pair (settings.go:16, 58) maps to `IngestConfig.PathTemplate` (config.go:416) and is already wired through `PromptAndSet` — the M5 #86 row above is for a *separate* server-controlled field, not a re-implementation of the existing one.
- `Settings.Snapshot`'s `RestartRequired` flag (settings.go:60) is the existing mechanism for fields that cannot be hot-reloaded — M5 #78's "live `Detector` restart" graduation path is what makes `ingest.cardRoots` a non-restart edit.
- `Settings.PromptAndSetIntegrationPath` (settings.go:143) is the model for any new per-list dialog (`PromptAndSetList` or similar) — a single parameterised method plus a `SettingsField` enum value, matching `PromptAndSet`'s own design.
