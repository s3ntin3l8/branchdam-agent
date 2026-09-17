# Tray settings menu — `SettingsView`-missing field inventory

Issue: #110 (part of #92 E3 — Tray reliability & UX, audit finding F-14).
Source audit: `docs/audit/2026-09-01-comprehensive-audit.md` (2026-09-01 audit, Section 3, F-14).
The audit doc is gitignored (`/docs/audit/` in `.gitignore`) and is local-only working material; this document and the audit comment block in [`internal/tray/settings.go`](../internal/tray/settings.go) are the canonical record.

This is an **inventory** issue, not a code change. Each field below is a sub-task that graduates to `SettingsView` (and the matching `SetBool` / `SetInt` / `SetString` wiring where needed) as its M5 / E3 sub-issue ships. One small PR per field — this document and the audit comment block in `settings.go` are updated in the same PR that adds the field, so the two never disagree.

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
to graduate.

**v1.9.0 update:** `internal/tray/integrationsmenu.go` and
`internal/tray/hooksmenu.go` are gone entirely, not just slimmed -- the
Wails window's `renderIntegrations`/`renderHooks` (live status, "Sync
now"/"Install"/"Reveal" actions) and `renderIntegrationBlock` (config
fields) now cover everything those two tray files did. `timeoutSecs`, the
one field the window didn't yet have when the note above was written, is
now `renderIntegrationBlock`'s own "Sync timeout" select, writing the same
`integrations.<id>.timeoutSecs` key the tray always did -- so there is no
longer a per-integration value reachable ONLY from the tray. The tray's
remaining surface is `Advanced` (`Reload config` / `Open config.yaml` /
`Reveal config folder`, plus a hidden `Restart now`; renamed from
`Settings` in the tray/window UX rethink since it no longer leads to any
actual setting) -- config-file affordances with no settings VALUE of their
own, same as before. The tray's own browsable status page (`Open status
page`) is gone in the same rethink -- the Wails window is now the only
settings/status surface; `/`, `/status`, and `/status.json` still serve
plain JSON for anyone who wants to `curl` them.

**Post-v1.11.0 note (structured list editors):** `cardRoots` and
`pathMappings` (rows below) went through a second graduation, this time
from a single comma-separated text input to a real structured editor in
the Wails window. `SettingsView.CardRoots []string` is new (there was no
`SettingsView` field for it at all before this -- the comma-separated box's
own placeholder used to read "current value not shown" for exactly that
reason); `app.js`'s `renderFolderListField` renders it as a repeatable
single-folder picker (Wails has no multi-select directory dialog) and
saves through the existing `SetStringSlice` array wire path, unchanged.
`pathMappings` gained a parallel structured representation,
`SettingsView.PathMappingEntries []tray.PathMappingEntry`, alongside the
existing formatted-string `PathMappings` field (kept, not replaced, so
nothing that reads the flat string breaks) -- because the comma/colon
string format `parsePathMappings` reads is lossy for a path containing a
comma, which a structured row editor makes easier to produce by accident.
A new `POST /api/settings/path-mappings` route and `configSettings.SetPathMappings`
(`cmd/branchdam-agent/settings.go`) write the structured form directly;
`SetString("pathMappings", ...)` still works for anything that still calls
it. `ingest.allowedExtensions` also moved off its comma-separated box to a
chip/token editor, still over the same `SetStringSlice` path as before.

**Post-#217 note:** `PromptAndSet`/`PromptAndSetIntegrationPath`/
`PromptAndSetIntegrationRewrites` and the `SettingsField` enum -- cited
below as the graduation mechanism for several rows, back when the tray's
own menu items were their only caller -- were removed once issue #211's
menu-slimming left them with no production caller at all. Every
`SettingsView` field and `Config` key those rows describe as "graduated"
is still graduated and still live; only the setter each row cites is
gone. Read every `PromptAndSet`/`FieldXxx` reference below as **historical**
(what wired the field at the time), not as a current API -- the live
mutators are `SetBool`/`SetInt`/`SetString`/`SetStringSlice`/
`SetIntegrationPath`/`SetIntegrationRewrites`, same as the Post-#211 note
above already established for the menu-item line numbers.

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
- **`tray.statusAddr` (config.go:190)** — loopback bind address. A non-loopback value exposes the unauthenticated status/settings JSON endpoints on the network. Restart-only and intentionally hand-edit.
- **`ingest.exiftoolPath`** — overrides which exiftool binary `internal/exiftool.Pool` invokes; empty (the default) resolves `exiftool` through PATH. Added alongside the exiftool `-stay_open` pooling refactor (#104), not part of this issue's original enumeration. A rarely-touched operator override, not worth a menu slot.

## Graduation workflow

When an M5 / E3 sub-issue lands:

1. Land the M5 / E3 sub-issue's own PR (adding the field to `Config` + implementing the runtime side of the feature).
2. Open a follow-up PR that:
   - Adds the field to `SettingsView`.
   - Wires the matching `SetBool` / `SetInt` / `SetString` (or a new list-editing method for `AllowedExtensions` / `AutoImportPaths`).
   - Updates the audit comment block in `settings.go` (removes graduated fields, keeps the deliberately-hand-edit list intact).
   - Updates the table in this doc (moves the row from "to graduate" to the "deliberately hand-edit only" section, or to a new "graduated" section).
   - Closes the matching sub-task on #110.
3. `make check` must pass for the follow-up PR; `Settings.SetString` for any new free-text field is the same test surface as the existing `server.baseUrl` / `server.apiKey` cases.

## Related context

- The existing `SettingsView.NamingTemplate` field (settings.go:64) maps to `IngestConfig.PathTemplate` (config.go:416) and is wired through `SetString("ingest.pathTemplate", ...)` — the M5 #86 row above is for a *separate* server-controlled field, not a re-implementation of the existing one.
- `Settings.Snapshot`'s `RestartRequired` flag is the existing mechanism for fields that cannot be hot-reloaded — M5 #78's "live `Detector` restart" graduation path is what makes `ingest.cardRoots` a non-restart edit.
- `Settings.SetIntegrationPath` (settings.go) is the model for any new per-integration field — a single parameterised method keyed by `IntegrationID`, rather than one method per integration.
